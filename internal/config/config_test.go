package config

import "testing"

func TestNormalizeHTTPBaseURLAddsDefaultScheme(t *testing.T) {
	got := normalizeHTTPBaseURL("185.200.65.95:31160")
	want := "http://185.200.65.95:31160"
	if got != want {
		t.Fatalf("normalizeHTTPBaseURL() got %q want %q", got, want)
	}
}

func TestNormalizeHTTPBaseURLKeepsExplicitScheme(t *testing.T) {
	got := normalizeHTTPBaseURL("https://example.com")
	want := "https://example.com"
	if got != want {
		t.Fatalf("normalizeHTTPBaseURL() got %q want %q", got, want)
	}
}

func TestNormalizeTCPAddressStripsURLScheme(t *testing.T) {
	got := normalizeTCPAddress("http://185.200.65.95:31160")
	want := "185.200.65.95:31160"
	if got != want {
		t.Fatalf("normalizeTCPAddress() got %q want %q", got, want)
	}
}

func TestNormalizeTCPAddressKeepsHostPort(t *testing.T) {
	got := normalizeTCPAddress("185.200.65.95:31160")
	want := "185.200.65.95:31160"
	if got != want {
		t.Fatalf("normalizeTCPAddress() got %q want %q", got, want)
	}
}
