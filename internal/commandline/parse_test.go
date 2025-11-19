package commandline

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseParameters(t *testing.T) {
	configDir := "/etc/bot-detector"
	logPath := "/var/log/access.log"
	stateDir := "/var/lib/bot-detector/state"
	expectConfigDir := "--config-dir <path> is required"
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
			args: []string{"bot-detector", "--config-dir", configDir, "--log-path", logPath},
			want: &AppParameters{
				ConfigDir: configDir,
				LogPath:   logPath,
				Envs:      &EnvParameters{},
			},
			wantErr: false,
		},
		{
			name: "all flags set",
			args: []string{
				"bot-detector",
				"--config-dir", configDir,
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
				Envs:         &EnvParameters{},
			},
			wantErr: false,
		},
		{
			name:    "version flag alone is valid",
			args:    []string{"bot-detector", "--version"},
			want:    &AppParameters{ShowVersion: true, Envs: &EnvParameters{}},
			wantErr: false,
		},
		{
			name:        "missing required log-path in live mode",
			args:        []string{"bot-detector", "--config-dir", configDir},
			wantErr:     true,
			errContains: expectLogPath,
		},
		{
			name:        "missing required config for live mode",
			args:        []string{"bot-detector", "--log-path", logPath},
			wantErr:     true,
			errContains: expectConfigDir,
		},
		{
			name:        "check requires config",
			args:        []string{"bot-detector", "--check"},
			wantErr:     true,
			errContains: expectConfigDir,
		},
		{
			name:        "dump-backends requires config",
			args:        []string{"bot-detector", "--dump-backends"},
			wantErr:     true,
			errContains: expectConfigDir,
		},
		{
			name: "dry-run without log-path is valid (reads from stdin)",
			args: []string{"bot-detector", "--config-dir", configDir, "--dry-run"},
			want: &AppParameters{
				ConfigDir: configDir,
				DryRun:    true,
				LogPath:   "", // LogPath should be empty
				Envs:      &EnvParameters{},
			},
			wantErr: false,
		},
		{
			name: "cluster node name flag",
			args: []string{"bot-detector", "--config-dir", configDir, "--log-path", logPath, "--cluster-node-name", "node-1"},
			want: &AppParameters{
				ConfigDir:       configDir,
				LogPath:         logPath,
				ClusterNodeName: "node-1",
				Envs:            &EnvParameters{},
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
				t.Errorf("ParseParameters() = got %+v, want %+v", got, tt.want)
			}
		})
	}
}
