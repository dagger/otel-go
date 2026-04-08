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
