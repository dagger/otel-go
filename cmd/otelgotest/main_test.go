package main

import (
	"reflect"
	"testing"
)

func TestBuildGoTestArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "no chdir",
			args: []string{"-run", "TestFoo", "./..."},
			want: []string{"test", "-json", "-run", "TestFoo", "./..."},
		},
		{
			name: "leading dash C separate arg",
			args: []string{"-C", "./foo/bar", "./..."},
			want: []string{"test", "-C", "./foo/bar", "-json", "./..."},
		},
		{
			name: "leading dash C equals form",
			args: []string{"-C=./foo/bar", "./..."},
			want: []string{"test", "-C=./foo/bar", "-json", "./..."},
		},
		{
			name: "leading dash C with explicit json",
			args: []string{"-C", "./foo/bar", "-json", "./..."},
			want: []string{"test", "-C", "./foo/bar", "-json", "./..."},
		},
		{
			name: "strips explicit json true",
			args: []string{"-json=true", "./..."},
			want: []string{"test", "-json", "./..."},
		},
		{
			name: "strips explicit json false",
			args: []string{"-json=false", "./..."},
			want: []string{"test", "-json", "./..."},
		},
		{
			name: "non-leading dash C is left in place",
			args: []string{"-run", "TestFoo", "-C", "./foo/bar", "./..."},
			want: []string{"test", "-json", "-run", "TestFoo", "-C", "./foo/bar", "./..."},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildGoTestArgs(tt.args)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("buildGoTestArgs(%q) = %q, want %q", tt.args, got, tt.want)
			}
		})
	}
}

func TestHasJSONFlag(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "bare flag", args: []string{"-json"}, want: true},
		{name: "explicit true", args: []string{"-json=true"}, want: true},
		{name: "explicit false", args: []string{"-json=false"}, want: false},
		{name: "stops at double dash", args: []string{"--", "-json"}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasJSONFlag(tt.args); got != tt.want {
				t.Fatalf("hasJSONFlag(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
