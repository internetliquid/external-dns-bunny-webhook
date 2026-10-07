package main

import (
	"os"
	"testing"
)

func TestCheckIncludeDomains(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		unset   bool
		wantErr bool
	}{
		{name: "set and empty", value: "", wantErr: true},
		{name: "blanks only", value: " , ,", wantErr: true},
		{name: "root only", value: ".", wantErr: true},
		{name: "root and blanks", value: " . , ", wantErr: true},
		{name: "unset", unset: true},
		{name: "normal list", value: "apps.dev.98dollarwebsite.com,apps.dev.98dollarblog.com"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BUNNY_INCLUDE_DOMAINS", tt.value)
			if tt.unset {
				os.Unsetenv("BUNNY_INCLUDE_DOMAINS")
			}

			err := checkIncludeDomains()
			if (err != nil) != tt.wantErr {
				t.Errorf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
