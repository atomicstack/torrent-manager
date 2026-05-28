package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveRestartExe_PrefersAbsoluteArgs0 pins the fix for the deploy
// gotcha that wedged the May 2026 seedctl restart: when an operator does
//
//	mv ~/bin/seedctl ~/bin/seedctl.bak.<ts>
//	mv ~/bin/seedctl.new ~/bin/seedctl
//	POST /v1/admin/restart
//
// os.Executable() reads /proc/self/exe, which points at the *renamed* (old)
// path, so a naive syscall.Exec re-execs the old binary in the same PID.
// argv[0] still carries the absolute launch path, which now resolves to the
// new file on disk — so we prefer it.
func TestResolveRestartExe_PrefersAbsoluteArgs0(t *testing.T) {
	fallback := func() (string, error) {
		// mimic os.Executable() returning the renamed-aside path
		return "/home/atmc/bin/seedctl.bak.20260528-175438", nil
	}
	got, err := resolveRestartExe([]string{"/home/atmc/bin/seedctl", "--config", "x.yaml"}, fallback)
	require.NoError(t, err)
	assert.Equal(t, "/home/atmc/bin/seedctl", got)
}

func TestResolveRestartExe_FallsBackWhenArgs0Relative(t *testing.T) {
	fallback := func() (string, error) {
		return "/abs/path/seedctl", nil
	}
	got, err := resolveRestartExe([]string{"./seedctl"}, fallback)
	require.NoError(t, err)
	assert.Equal(t, "/abs/path/seedctl", got)
}

func TestResolveRestartExe_FallsBackWhenArgs0Bare(t *testing.T) {
	// argv[0] is just the basename when invoked via $PATH lookup; not
	// absolute, so must fall back to the os.Executable() resolver.
	fallback := func() (string, error) {
		return "/abs/seedctl", nil
	}
	got, err := resolveRestartExe([]string{"seedctl"}, fallback)
	require.NoError(t, err)
	assert.Equal(t, "/abs/seedctl", got)
}

func TestResolveRestartExe_NoArgs_UsesFallback(t *testing.T) {
	fallback := func() (string, error) {
		return "/abs/seedctl", nil
	}
	got, err := resolveRestartExe(nil, fallback)
	require.NoError(t, err)
	assert.Equal(t, "/abs/seedctl", got)
}

func TestResolveRestartExe_FallbackErrorPropagates(t *testing.T) {
	fallback := func() (string, error) {
		return "", errors.New("readlink /proc/self/exe: i/o error")
	}
	_, err := resolveRestartExe([]string{"./seedctl"}, fallback)
	require.Error(t, err)
}

// TestDefaultItemsDBPath_UsesHome pins the second half of the deploy
// fallout: the previous default was the hardcoded fictional path
// `/home/seedbox/etc/seedctl/items.db` (introduced by the privacy scrub),
// which broke startup on the real seedbox the moment the prod config
// didn't override it. The default must derive from $HOME so the binary
// works wherever it lands.
func TestDefaultItemsDBPath_UsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := defaultItemsDBPath()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(home, "etc", "seedctl", "items.db"), p)
}
