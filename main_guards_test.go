package main

import "testing"

func TestRefuseImplicitDevMode(t *testing.T) {
	tests := []struct {
		name     string
		kService string
		devMode  string
		isDev    bool
		want     bool
	}{
		{name: "cloud run with implicit dev mode", kService: "prism", devMode: "", isDev: true, want: true},
		{name: "cloud run with DEV_MODE set to something else", kService: "prism", devMode: "yes", isDev: true, want: true},
		{name: "cloud run with explicit dev mode", kService: "prism", devMode: "true", isDev: true, want: false},
		{name: "cloud run in production mode", kService: "prism", devMode: "", isDev: false, want: false},
		{name: "local implicit dev mode", kService: "", devMode: "", isDev: true, want: false},
		{name: "local production mode", kService: "", devMode: "", isDev: false, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := refuseImplicitDevMode(tt.kService, tt.devMode, tt.isDev); got != tt.want {
				t.Errorf("refuseImplicitDevMode(%q, %q, %v) = %v, want %v", tt.kService, tt.devMode, tt.isDev, got, tt.want)
			}
		})
	}
}
