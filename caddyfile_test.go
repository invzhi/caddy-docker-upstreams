package caddy_docker_upstreams

import (
	"testing"

	"github.com/caddyserver/caddy/v2/caddyconfig/caddyfile"
	"github.com/stretchr/testify/assert"
)

func TestUnmarshalCaddyfile(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{
			name:  "bare directive",
			input: `docker`,
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
			}
		})
	}
}
