package commandline

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestParseParameters(t *testing.T) {
	// Create a temporary directory for testing
	tmpDir := t.TempDir()
	configDir := filepath.Join(tmpDir, "config")
	if err := os.Mkdir(configDir, 0755); err != nil {
		t.Fatalf("Failed to create test config directory: %v", err)
	}

	configPath := filepath.Join(configDir, "config.yaml")
	logPath := filepath.Join(tmpDir, "access.log")
	stateDir := filepath.Join(tmpDir, "state")
	if err := os.Mkdir(stateDir, 0755); err != nil {
		t.Fatalf("Failed to create test state directory: %v", err)
	}

	expectConfigPath := "--config <path> is required"
	expectLogPath := "--log-path <path> is required"
	tests := []struct {
		name        string
		args        []string
		want        *AppParameters
		wantErr     bool
		errContains string
	}{
		{
			name: "live mode (basic valid flags)",
			args: []string{"bot-detector", "--config", configPath, "--log-path", logPath},
			want: &AppParameters{
				ConfigPath: configPath,
				ConfigDir:  configDir,
				LogPath:    logPath,
			},
			wantErr: false,
		},
		{
			name: "all flags set",
			args: []string{
				"bot-detector",
				"--config", configPath,
				"--log-path", logPath,
				"--state-dir", stateDir,
				"--dry-run",
				"--exit-on-eof",
				"--version",
				"--check",
				"--dump-backends",
				"--reload-on", "hup",
				"--top-n", "15",
				"--http-server", ":9090",
			},
			want: &AppParameters{
				ConfigPath:   configPath,
				ConfigDir:    configDir,
				LogPath:      logPath,
				StateDir:     stateDir,
				DryRun:       true,
				ExitOnEOF:    true,
				ShowVersion:  true,
				Check:        true,
				DumpBackends: true,
				ReloadOn:     "hup",
				TopN:         15,
				HTTPServer:   ":9090",
			},
			wantErr: false,
		},
		{
			name:    "version flag alone is valid",
			args:    []string{"bot-detector", "--version"},
			want:    &AppParameters{ShowVersion: true},
			wantErr: false,
		},
		{
			name:        "missing required log-path in live mode",
			args:        []string{"bot-detector", "--config", configPath},
			wantErr:     true,
			errContains: expectLogPath,
		},
		{
			name:        "missing required config for live mode",
			args:        []string{"bot-detector", "--log-path", logPath},
			wantErr:     true,
			errContains: expectConfigPath,
		},
		{
			name:        "check requires config",
			args:        []string{"bot-detector", "--check"},
			wantErr:     true,
			errContains: expectConfigPath,
		},
		{
			name:        "dump-backends requires config",
			args:        []string{"bot-detector", "--dump-backends"},
			wantErr:     true,
			errContains: expectConfigPath,
		},
		{
			name: "dry-run without log-path is valid (reads from stdin)",
			args: []string{"bot-detector", "--config", configPath, "--dry-run"},
			want: &AppParameters{
				ConfigPath: configPath,
				ConfigDir:  configDir,
				DryRun:     true,
				LogPath:    "", // LogPath should be empty
			},
			wantErr: false,
		},
		{
			name:        "no flags returns help error",
			args:        []string{"bot-detector"},
			wantErr:     true,
			errContains: "no flag: help requested",
		},
		{
			name: "config directory path builds config.yaml path",
			args: []string{"bot-detector", "--config", configDir, "--log-path", logPath},
			want: &AppParameters{
				ConfigPath: configPath,
				ConfigDir:  configDir,
				LogPath:    logPath,
			},
			wantErr: false,
		},
		{
			name:        "config file with wrong name fails",
			args:        []string{"bot-detector", "--config", filepath.Join(configDir, "wrong.yaml"), "--log-path", logPath},
			wantErr:     true,
			errContains: "config file must be named 'config.yaml'",
		},
		{
			name:        "non-existent config directory fails",
			args:        []string{"bot-detector", "--config", filepath.Join(tmpDir, "nonexistent"), "--log-path", logPath},
			wantErr:     true,
			errContains: "config directory does not exist",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseParameters(tt.args)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseParameters() error = nil, wantErr %v", tt.wantErr)
				}
				if tt.errContains != "" && !strings.Contains(err.Error(), tt.errContains) {
					t.Errorf("ParseParameters() error = %q, want error containing %q", err, tt.errContains)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseParameters() unexpected error = %v", err)
			}

			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ParseParameters() = got %+v, want %+v", got, tt.want)
			}
		})
	}
}
