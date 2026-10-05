#define AppVersion GetEnv("VERSION")
#define AppArch GetEnv("ARCH")
#define RepoRoot AddBackslash(SourcePath) + "..\.."
#if Pos("-", AppVersion) > 0
  #define AppNumericVersion Copy(AppVersion, 1, Pos("-", AppVersion) - 1)
#else
  #define AppNumericVersion AppVersion
#endif

#if AppVersion == ""
  #error VERSION is required (for example, 0.2.0-beta.1)
#endif
#if AppArch != "amd64"
  #error ARCH must be amd64
#endif

[Setup]
AppId={{DDF67207-8F8A-4AE0-BAA8-472711E78643}
AppName={cm:ProductName}
AppVersion={#AppVersion}
AppVerName={cm:ProductName} {#AppVersion}
AppPublisher=Tamias
DefaultDirName={localappdata}\Programs\tamias
DefaultGroupName=Tamias
DisableProgramGroupPage=yes
DisableWelcomePage=no
LicenseFile="{#RepoRoot}\LICENSE"
OutputDir="{#RepoRoot}\dist"
OutputBaseFilename=tamias-{#AppVersion}-windows-{#AppArch}-setup
SetupIconFile="{#SourcePath}\tamias.ico"
UninstallDisplayIcon={app}\tamias.ico
ArchitecturesAllowed=x64
ArchitecturesInstallIn64BitMode=x64
PrivilegesRequired=lowest
CloseApplications=yes
RestartApplications=no
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
VersionInfoCompany=Tamias
VersionInfoProductName=Tamias
VersionInfoDescription=Tamias Setup
VersionInfoVersion={#AppNumericVersion}
VersionInfoProductVersion={#AppNumericVersion}
VersionInfoTextVersion={#AppVersion}
VersionInfoProductTextVersion={#AppVersion}
VersionInfoCopyright=Copyright (C) tamiops contributors
SetupLogging=yes

[Languages]
Name: "english"; MessagesFile: "compiler:Default.isl"
; Vendored from jrsoftware/issrc commit 97c4aa92235f44d0ee6617d739eb9ab0bdb404bf; upstream attribution is preserved in the file.
Name: "chinesesimp"; MessagesFile: "{#SourcePath}\ChineseSimplified.isl"

[CustomMessages]
english.ProductName=Tamias
chinesesimp.ProductName=小花鼠 Tamias
english.WebView2Missing=The Microsoft Edge WebView2 Runtime is required to run Tamias. Open Microsoft's official download page now? Install the Evergreen Runtime there, then run this setup again.
english.WebView2OpenFailed=The browser could not be opened. Visit https://developer.microsoft.com/microsoft-edge/webview2/ to install the Microsoft Edge WebView2 Runtime, then run this setup again.
english.DesktopTask=Create a desktop shortcut
english.ShortcutGroup=Additional shortcuts:
english.LaunchProgram=Launch Tamias
chinesesimp.WebView2Missing=运行小花鼠需要 Microsoft Edge WebView2 Runtime。现在打开 Microsoft 官方下载页面吗？请在那里安装 Evergreen Runtime，然后重新运行此安装程序。
chinesesimp.WebView2OpenFailed=无法打开浏览器。请访问 https://developer.microsoft.com/microsoft-edge/webview2/ 安装 Microsoft Edge WebView2 Runtime，然后重新运行此安装程序。
chinesesimp.DesktopTask=创建桌面快捷方式
chinesesimp.ShortcutGroup=其他快捷方式：
chinesesimp.LaunchProgram=启动小花鼠

[Tasks]
Name: "desktopicon"; Description: "{cm:DesktopTask}"; GroupDescription: "{cm:ShortcutGroup}"; Flags: unchecked

[Files]
Source: "{#RepoRoot}\bin\tamias.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#SourcePath}\tamias.ico"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#RepoRoot}\LICENSE"; DestDir: "{app}"; Flags: ignoreversion
Source: "{#RepoRoot}\THIRD_PARTY_NOTICES.txt"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\Tamias"; Filename: "{app}\tamias.exe"; WorkingDir: "{app}"; IconFilename: "{app}\tamias.ico"
Name: "{autodesktop}\Tamias"; Filename: "{app}\tamias.exe"; WorkingDir: "{app}"; IconFilename: "{app}\tamias.ico"; Tasks: desktopicon

[Run]
Filename: "{app}\tamias.exe"; Description: "{cm:LaunchProgram}"; Flags: postinstall nowait skipifsilent unchecked

[Code]
function IsPositiveVersion(const Version: String): Boolean;
var
  I, Parts: Integer;
  HasDigit, HasNonZero: Boolean;
  Ch: Char;
begin
  Result := False;
  if Version = '' then
    Exit;

  Parts := 0;
  HasDigit := False;
  HasNonZero := False;
  for I := 1 to Length(Version) do begin
    Ch := Version[I];
    if (Ch >= '0') and (Ch <= '9') then begin
      HasDigit := True;
      if Ch <> '0' then
        HasNonZero := True;
    end else if Ch = '.' then begin
      if not HasDigit then
        Exit;
      Inc(Parts);
      HasDigit := False;
    end else
      Exit;
  end;
  if not HasDigit then
    Exit;
  Inc(Parts);
  Result := (Parts = 4) and HasNonZero;
end;

function IsWebView2RuntimeInstalled: Boolean;
var
  Version: String;
  RuntimeKey: String;
begin
  Result := False;
  RuntimeKey := 'Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}';
  if RegQueryStringValue(HKEY_LOCAL_MACHINE_32, RuntimeKey, 'pv', Version) and IsPositiveVersion(Version) then begin
    Result := True;
    Exit;
  end;

  if RegQueryStringValue(HKEY_CURRENT_USER_64, RuntimeKey, 'pv', Version) and IsPositiveVersion(Version) then
    Result := True;
end;

function InitializeSetup: Boolean;
var
  ErrorCode: Integer;
begin
  Result := IsWebView2RuntimeInstalled;
  if Result then
    Exit;

  if WizardSilent then begin
    Log('Microsoft Edge WebView2 Runtime is missing; refusing silent installation.');
    Result := False;
    Exit;
  end;

  if MsgBox(CustomMessage('WebView2Missing'), mbInformation, MB_YESNO) = IDYES then begin
    if not ShellExec('open', 'https://developer.microsoft.com/microsoft-edge/webview2/', '', '', SW_SHOWNORMAL, ewNoWait, ErrorCode) then
      MsgBox(CustomMessage('WebView2OpenFailed'), mbError, MB_OK);
  end;
  Result := False;
end;
