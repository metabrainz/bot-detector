package commandline

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseParameters(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		want        *AppParameters
		wantErr     bool
		errContains string
	}{
		{
			name: "live mode (basic valid flags)",
			args: []string{"bot-detector", "--config", "myconfig.yaml", "--log-path", "/var/log/access.log"},
			want: &AppParameters{
				ConfigPath: "myconfig.yaml",
				LogPath:    "/var/log/access.log",
			},
			wantErr: false,
		},
		{
			name: "all flags set",
			args: []string{
				"bot-detector",
				"--config", "c.yaml",
				"--log-path", "l.log",
				"--state-dir", "/state",
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
				ConfigPath:   "c.yaml",
				LogPath:      "l.log",
				StateDir:     "/state",
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
			args:        []string{"bot-detector", "--config", "myconfig.yaml"},
			wantErr:     true,
			errContains: "--log-path is required in live mode",
		},
		{
			name:        "missing required config for live mode",
			args:        []string{"bot-detector", "--log-path", "/var/log/access.log"},
			wantErr:     true,
			errContains: "--config flag is required",
		},
		{
			name:        "check requires config",
			args:        []string{"bot-detector", "--check"},
			wantErr:     true,
			errContains: "--config flag is required for --check",
		},
		{
			name:        "dump-backends requires config",
			args:        []string{"bot-detector", "--dump-backends"},
			wantErr:     true,
			errContains: "--config flag is required for --dump-backends",
		},
		{
			name: "dry-run without log-path is valid",
			args: []string{"bot-detector", "--config", "c.yaml", "--dry-run"},
			want: &AppParameters{
				ConfigPath: "c.yaml",
				DryRun:     true,
			},
			wantErr: false,
		},
		{
			name:        "no flags returns help error",
			args:        []string{"bot-detector"},
			wantErr:     true,
			errContains: "no flag: help requested",
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
				t.Errorf("ParseParameters() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
