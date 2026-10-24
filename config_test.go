package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseChatMap(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want map[string][]string
	}{
		{
			name: "single string id",
			raw:  `{"andy@example.com":"123456789"}`,
			want: map[string][]string{"andy@example.com": {"123456789"}},
		},
		{
			name: "bare number id",
			raw:  `{"andy@example.com":123456789}`,
			want: map[string][]string{"andy@example.com": {"123456789"}},
		},
		{
			name: "list of ids and duplicates removed",
			raw:  `{"team@example.com":["111","222","111"]}`,
			want: map[string][]string{"team@example.com": {"111", "222"}},
		},
		{
			name: "keys and values are normalised",
			raw:  `{" Andy@Example.com ":" 123456789 "}`,
			want: map[string][]string{"andy@example.com": {"123456789"}},
		},
		{
			name: "negative group id",
			raw:  `{"team@example.com":-1001234567890}`,
			want: map[string][]string{"team@example.com": {"-1001234567890"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseChatMap(tt.raw)
			if err != nil {
				t.Fatalf("parseChatMap(%q): %v", tt.raw, err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("parseChatMap(%q) = %#v, want %#v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestParseChatMapRejects(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{name: "not json", raw: `andy@example.com:123456789`, wantErr: "must be a JSON object"},
		{name: "empty object", raw: `{}`, wantErr: "at least one"},
		{name: "empty email", raw: `{" ":"111"}`, wantErr: "empty email"},
		{name: "empty id", raw: `{"a@example.com":""}`, wantErr: "empty chat id"},
		{name: "empty list", raw: `{"a@example.com":[]}`, wantErr: "no chat ids"},
		{name: "id with whitespace", raw: `{"a@example.com":"1 2"}`, wantErr: "invalid chat id"},
		{name: "duplicate email differing in case", raw: `{"a@example.com":"1","A@example.com":"2"}`, wantErr: "more than once"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseChatMap(tt.raw)
			if err == nil {
				t.Fatalf("parseChatMap(%q) succeeded, want error", tt.raw)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("parseChatMap(%q) error = %q, want it to contain %q", tt.raw, err, tt.wantErr)
			}
		})
	}
}

func TestChatsForIsCaseInsensitive(t *testing.T) {
	cfg := &config{telegramChats: map[string][]string{
		"andy@example.com": {"123456789"},
	}}

	for _, email := range []string{"andy@example.com", "Andy@Example.com", " ANDY@EXAMPLE.COM "} {
		got := cfg.chatsFor(email)
		if len(got) != 1 || got[0] != "123456789" {
			t.Fatalf("chatsFor(%q) = %#v, want [123456789]", email, got)
		}
	}
	if got := cfg.chatsFor("someone-else@example.com"); got != nil {
		t.Fatalf("chatsFor(unknown) = %#v, want nil", got)
	}
}

func TestTestTarget(t *testing.T) {
	single := &config{telegramChats: map[string][]string{"andy@example.com": {"123456789"}}}
	email, chats, err := single.testTarget("")
	if err != nil {
		t.Fatalf("testTarget(\"\") on single-entry map: %v", err)
	}
	if email != "andy@example.com" || len(chats) != 1 || chats[0] != "123456789" {
		t.Fatalf("testTarget(\"\") = %q, %#v", email, chats)
	}

	multi := &config{telegramChats: map[string][]string{
		"andy@example.com": {"1"},
		"team@example.com": {"2"},
	}}
	if _, _, err := multi.testTarget(""); err == nil {
		t.Fatal("testTarget(\"\") on multi-entry map should fail")
	}
	email, chats, err = multi.testTarget("TEAM@example.com")
	if err != nil {
		t.Fatalf("testTarget with explicit address: %v", err)
	}
	if email != "team@example.com" || len(chats) != 1 || chats[0] != "2" {
		t.Fatalf("testTarget explicit = %q, %#v", email, chats)
	}
	if _, _, err := multi.testTarget("nobody@example.com"); err == nil {
		t.Fatal("testTarget with unmapped address should fail")
	}
}
