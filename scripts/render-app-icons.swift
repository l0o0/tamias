import AppKit
import Foundation

let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent().deletingLastPathComponent()
let appSVG = root.appendingPathComponent("build/assets/app-icon.svg")
let traySVG = root.appendingPathComponent("frontend/src/assets/squirrel-logo.svg")

func renderPNG(from path: URL, size: Int) throws -> Data {
    guard let image = NSImage(contentsOf: path) else {
        throw NSError(domain: "IconRenderer", code: 1, userInfo: [NSLocalizedDescriptionKey: "Cannot read SVG at \(path.path)"])
    }
    guard let bitmap = NSBitmapImageRep(
        bitmapDataPlanes: nil,
        pixelsWide: size,
        pixelsHigh: size,
        bitsPerSample: 8,
        samplesPerPixel: 4,
        hasAlpha: true,
        isPlanar: false,
        colorSpaceName: .deviceRGB,
        bytesPerRow: 0,
        bitsPerPixel: 0
    ), let context = NSGraphicsContext(bitmapImageRep: bitmap) else {
        throw NSError(domain: "IconRenderer", code: 2, userInfo: [NSLocalizedDescriptionKey: "Cannot create a transparent \(size)×\(size) bitmap"])
    }

    bitmap.size = NSSize(width: size, height: size)
    context.imageInterpolation = .high
    NSGraphicsContext.saveGraphicsState()
    NSGraphicsContext.current = context
    context.cgContext.clear(CGRect(x: 0, y: 0, width: size, height: size))
    image.draw(
        in: NSRect(x: 0, y: 0, width: size, height: size),
        from: NSRect(origin: .zero, size: image.size),
        operation: .copy,
        fraction: 1
    )
    context.flushGraphics()
    NSGraphicsContext.restoreGraphicsState()

    guard let png = bitmap.representation(using: .png, properties: [:]) else {
        throw NSError(domain: "IconRenderer", code: 3, userInfo: [NSLocalizedDescriptionKey: "Cannot encode the rendered icon as PNG"])
    }
    return png
}

func littleEndian<T: FixedWidthInteger>(_ value: T) -> [UInt8] {
    var encoded = value.littleEndian
    return withUnsafeBytes(of: &encoded, Array.init)
}

func makeWindowsIcon(frames: [(size: Int, png: Data)]) -> Data {
    let headerLength = 6 + 16 * frames.count
    var offset = headerLength
    var result = Data([0, 0, 1, 0, UInt8(frames.count), 0])
    for frame in frames {
        result.append(UInt8(frame.size == 256 ? 0 : frame.size))
        result.append(UInt8(frame.size == 256 ? 0 : frame.size))
        result.append(contentsOf: [0, 0])
        result.append(contentsOf: littleEndian(UInt16(1)))
        result.append(contentsOf: littleEndian(UInt16(32)))
        result.append(contentsOf: littleEndian(UInt32(frame.png.count)))
        result.append(contentsOf: littleEndian(UInt32(offset)))
        offset += frame.png.count
    }
    for frame in frames {
        result.append(frame.png)
    }
    return result
}

// CSS ease-in-out uses cubic-bezier(0.42, 0, 0.58, 1).
func easeInOut(_ progress: Double) -> Double {
    func coordinate(_ t: Double, _ first: Double, _ second: Double) -> Double {
        let inverse = 1 - t
        return 3 * inverse * inverse * t * first + 3 * inverse * t * t * second + t * t * t
    }
    var lower = 0.0
    var upper = 1.0
    for _ in 0..<24 {
        let t = (lower + upper) / 2
        if coordinate(t, 0.42, 0.58) < progress {
            lower = t
        } else {
            upper = t
        }
    }
    return coordinate((lower + upper) / 2, 0, 1)
}

func renderTrayAnimation() throws {
    let keyframes: [(time: Double, angle: Double)] = [
        (0, 0), (0.20, -7), (0.43, 5), (0.64, -3.5), (0.82, 1.5), (1, 0)
    ]
    let frameCount = 20
    let source = try String(contentsOf: traySVG, encoding: .utf8)
        .replacingOccurrences(of: "<style>[\\s\\S]*?</style>", with: "", options: .regularExpression)
    let tailGroup = "<g class=\"tm-logo-tail\">"
    guard source.contains(tailGroup) else {
        throw NSError(domain: "IconRenderer", code: 4, userInfo: [NSLocalizedDescriptionKey: "Cannot find the tray icon's tail group"])
    }

    let fileManager = FileManager()
    let directory = root.appendingPathComponent("build/assets/tray-tail", isDirectory: true)
    try fileManager.createDirectory(at: directory, withIntermediateDirectories: true)
    let temporaryDirectory = fileManager.temporaryDirectory
        .appendingPathComponent("tamias-tray-tail-\(UUID().uuidString)", isDirectory: true)
    try fileManager.createDirectory(at: temporaryDirectory, withIntermediateDirectories: true)
    defer { try? fileManager.removeItem(at: temporaryDirectory) }

    for frame in 0..<frameCount {
        let progress = Double(frame) / Double(frameCount - 1)
        var angle = 0.0
        if frame != 0 && frame != frameCount - 1 {
            for index in 0..<(keyframes.count - 1) {
                let start = keyframes[index]
                let end = keyframes[index + 1]
                if progress >= start.time && progress <= end.time {
                    let easedProgress = easeInOut((progress - start.time) / (end.time - start.time))
                    angle = start.angle + (end.angle - start.angle) * easedProgress
                    break
                }
            }
        }
        // Rotate only the tail, preserving the source viewBox and every other group.
        let svg = source.replacingOccurrences(
            of: tailGroup,
            with: "<g class=\"tm-logo-tail\" transform=\"rotate(\(angle) 78 135)\">"
        )
        let svgPath = temporaryDirectory.appendingPathComponent("frame-\(frame).svg")
        try svg.write(to: svgPath, atomically: true, encoding: .utf8)
        let png = try renderPNG(from: svgPath, size: 160)
        let name = String(format: "frame-%02d.png", frame)
        try png.write(to: directory.appendingPathComponent(name), options: .atomic)
    }
    print("Rendered 20 transparent 160×160 tray tail frames (1050 ms greeting).")
}

do {
    if !CommandLine.arguments.contains("--tray-animation-only") {
        let appPNG = try renderPNG(from: appSVG, size: 1024)
        try appPNG.write(to: root.appendingPathComponent("build/assets/app-icon.png"), options: .atomic)
        try appPNG.write(to: root.appendingPathComponent("frontend/public/app-icon.png"), options: .atomic)

        let trayPNG = try renderPNG(from: traySVG, size: 1024)
        try trayPNG.write(to: root.appendingPathComponent("build/assets/tray-icon.png"), options: .atomic)

        let frames = try [16, 32, 48, 64, 128, 256].map { size in
            (size: size, png: try renderPNG(from: appSVG, size: size))
        }
        try makeWindowsIcon(frames: frames).write(to: root.appendingPathComponent("build/windows/tamias.ico"), options: .atomic)
        print("Rendered transparent app PNGs, monochrome tray PNG, and Windows ICO.")
    }
    try renderTrayAnimation()
} catch {
    fputs("Icon rendering failed: \(error.localizedDescription)\n", stderr)
    exit(1)
}
