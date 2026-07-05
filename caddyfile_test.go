package caddy_docker_upstreams

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/stretchr/testify/assert"
)

func TestUnmarshalCaddyfile(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantErr    bool
		wantLabels map[string][]string
	}{
		{
			name:  "bare directive",
			input: `docker`,
		},
		{
			name: "empty block",
			input: `docker {
			}`,
		},
		{
			name:    "unexpected argument",
			input:   `docker foo`,
			wantErr: true,
		},
		{
			name: "unrecognized block option",
			input: `docker {
				foo
			}`,
			wantErr: true,
		},
		{
			name: "single label",
			input: `docker {
				label com.docker.compose.service first
			}`,
			wantLabels: map[string][]string{
				"com.docker.compose.service": {"first"},
			},
		},
		{
			name: "label with multiple values",
			input: `docker {
				label com.docker.compose.service first second
			}`,
			wantLabels: map[string][]string{
				"com.docker.compose.service": {"first", "second"},
			},
		},
		{
			name: "multiple label directives",
			input: `docker {
				label com.docker.compose.service first
				label com.docker.compose.project demo
			}`,
			wantLabels: map[string][]string{
				"com.docker.compose.service": {"first"},
				"com.docker.compose.project": {"demo"},
			},
		},
		{
			name: "repeated label key unions values",
			input: `docker {
				label com.docker.compose.service first
				label com.docker.compose.service second
			}`,
			wantLabels: map[string][]string{
				"com.docker.compose.service": {"first", "second"},
			},
		},
		{
			name: "label without value",
			input: `docker {
				label com.docker.compose.service
			}`,
			wantErr: true,
		},
		{
			name: "label without arguments",
			input: `docker {
				label
			}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := caddyfile.NewTestDispenser(tt.input)

			var u Upstreams
			err := u.UnmarshalCaddyfile(d)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantLabels, u.Labels)
			}
		})
	}
}
