package main

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServe_migrateOnStart pins the flag/env wiring: default true, overridable
// by SERVE_MIGRATE_ON_START, and the flag wins over the environment.
func TestServe_migrateOnStart(t *testing.T) {
	flag := serveCmd.Flags().Lookup("migrate-on-start")
	require.NotNil(t, flag)
	t.Cleanup(func() {
		require.NoError(t, flag.Value.Set(flag.DefValue))
		flag.Changed = false
	})

	assert.True(t, viper.GetBool("serve-migrate-on-start"), "default must be true")

	t.Setenv("SERVE_MIGRATE_ON_START", "false")
	assert.False(t, viper.GetBool("serve-migrate-on-start"), "env must override default")

	require.NoError(t, serveCmd.ParseFlags([]string{"--migrate-on-start=true"}))
	assert.True(t, viper.GetBool("serve-migrate-on-start"), "flag must override env")

	require.NoError(t, serveCmd.ParseFlags([]string{"--migrate-on-start=false"}))
	assert.False(t, viper.GetBool("serve-migrate-on-start"))
}
