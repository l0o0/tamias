// Command tamias hosts the same storage core without Wails, a webview or a desktop session.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"tamiops/internal/core"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	flags := flag.NewFlagSet("tamias", flag.ContinueOnError)
	base, _ := os.UserConfigDir()
	data := flags.String("data-dir", filepath.Join(base, "Tami"), "State directory (exclusive per process)")
	gateways := flags.String("gateways", "", "Comma-separated gateway IDs for serve")
	verify := flags.Bool("verify-writes", false, "Probe conditional write/delete support before enabling scheduled writes")
	input := flags.String("input", "", "JSON configuration input file")
	jobID := flags.String("job", "", "Sync job ID")
	token := flags.String("token", "", "Preview token for run")
	confirm := flags.Bool("confirm-deletes", false, "Confirm deletion list from preview")
	version := flags.Bool("version", false, "Print application version and exit")
	if err := flags.Parse(os.Args[1:]); err != nil {
		return err
	}
	if *version {
		fmt.Println(core.Version)
		return nil
	}
	command := "status"
	if flags.NArg() > 0 {
		command = flags.Arg(0)
	}
	var vault core.Vault
	if key, err := vaultKeyFromEnv(); err != nil {
		return err
	} else if key != nil {
		vault, err = core.NewEncryptedVault(filepath.Join(*data, "credentials"), key)
		if err != nil {
			return err
		}
	}
	service, err := core.New(*data, vault)
	if err != nil {
		return err
	}
	defer service.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if *verify {
		for _, c := range service.Snapshot()["connections"].([]core.Connection) {
			if _, err = service.TestConnection(ctx, c.ID, true); err != nil {
				return fmt.Errorf("写入验证失败 %s: %w", c.Name, err)
			}
		}
	}
	var result any
	switch command {
	case "status":
		result = service.Snapshot()
	case "export":
		result, err = service.ExportConfiguration()
	case "diagnostics":
		result = service.Diagnostics()
	case "import":
		raw, err := os.ReadFile(*input)
		if err != nil {
			return err
		}
		var v core.ConfigurationExport
		if err = json.Unmarshal(raw, &v); err != nil {
			return err
		}
		result, err = service.ImportConfiguration(v)
		if err != nil {
			return err
		}
	case "connection-add", "credentials":
		raw, err := os.ReadFile(*input)
		if err != nil {
			return err
		}
		var v core.ConnectionInput
		if err = json.Unmarshal(raw, &v); err != nil {
			return err
		}
		if command == "credentials" {
			err = service.UpdateCredentials(ctx, v)
		} else {
			result, err = service.AddConnection(ctx, v)
		}
		if err != nil {
			return err
		}
	case "preview":
		result, err = service.Preview(ctx, *jobID)
	case "run":
		result, err = service.RunPlan(ctx, *jobID, *token, *confirm)
	case "serve":
		service.StartScheduler()
		service.StartMaintenance()
		for _, id := range strings.Split(*gateways, ",") {
			if id = strings.TrimSpace(id); id != "" {
				if err = service.StartGateway(id); err != nil {
					return err
				}
			}
		}
		fmt.Fprintln(os.Stderr, "Tamias core running; press Ctrl+C to stop")
		<-ctx.Done()
		return nil
	default:
		return fmt.Errorf("命令：status, export, import, connection-add, credentials, diagnostics, preview, run, serve；参数放在命令前")
	}
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(result)
}

func vaultKeyFromEnv() ([]byte, error) {
	name, value := "TAMIAS_VAULT_KEY", os.Getenv("TAMIAS_VAULT_KEY")
	if value == "" {
		name, value = "TAMIOPS_VAULT_KEY", os.Getenv("TAMIOPS_VAULT_KEY")
	}
	if value == "" {
		name, value = "TAMI_VAULT_KEY", os.Getenv("TAMI_VAULT_KEY")
	}
	if value == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s 应为 base64 编码的 32 字节密钥", name)
	}
	return key, nil
}
