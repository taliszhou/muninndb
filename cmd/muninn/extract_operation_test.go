package main

import (
	"testing"
)

// TestExtractOperation_FlagParsing verifies that extractOperation correctly
// identifies the operation name when flags with and without values are present.
// Note: without a flag registry the parser cannot distinguish a boolean flag
// from one that takes a value purely by position, so this test covers the
// cases that are deterministically solvable.
func TestExtractOperation_FlagParsing(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantOp  string
		wantLen int // expected length of returned rest slice
	}{
		{
			name:    "op only",
			args:    []string{"remember"},
			wantOp:  "remember",
			wantLen: 0,
		},
		{
			name:    "flag=value before op",
			args:    []string{"--vault=default", "remember"},
			wantOp:  "remember",
			wantLen: 1,
		},
		{
			name:    "flag value before op",
			args:    []string{"--vault", "default", "remember"},
			wantOp:  "remember",
			wantLen: 2,
		},
		{
			name:    "op followed by flags",
			args:    []string{"remember", "--concept", "c", "--content", "body"},
			wantOp:  "remember",
			wantLen: 4,
		},
		{
			// Before the fix: --verbose consumed --data-dir as its value (i+=2
			// with no lookahead), so "/tmp" was returned as the operation name.
			// After the fix, --verbose detects that its next token is also a flag
			// and skips itself only (i++), allowing --data-dir to consume "/tmp"
			// correctly and "remember" to be identified as the operation.
			name:    "boolean flag before value-flag — regression guard",
			args:    []string{"--verbose", "--data-dir", "/tmp", "remember"},
			wantOp:  "remember",
			wantLen: 3,
		},
		{
			// Flag at end of args with no value or operation following — must not
			// panic and must return an empty operation cleanly.
			name:    "flag at end no op",
			args:    []string{"--data-dir"},
			wantOp:  "",
			wantLen: 1,
		},
		{
			name:    "no args",
			args:    []string{},
			wantOp:  "",
			wantLen: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotOp, gotRest := extractOperation(tt.args)
			if gotOp != tt.wantOp {
				t.Errorf("op: got %q, want %q (args: %v)", gotOp, tt.wantOp, tt.args)
			}
			if len(gotRest) != tt.wantLen {
				t.Errorf("rest len: got %d, want %d (rest: %v)", len(gotRest), tt.wantLen, gotRest)
			}
		})
	}
}
