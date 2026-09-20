package main

import "github.com/digitalocean/tester"

type config struct {
	Packages  []*tester.Package `json:"packages"`
	Scheduler *schedulerConfig  `json:"scheduler"`
	Slack     *slackConfig      `json:"slack"`
}

type schedulerConfig struct {
	// RunTimeout is how long a claimed run may go without finishing before
	// it is reset (Go duration string). Default 15m.
	RunTimeout string `json:"run_timeout"`
	// RunDelay is the default minimum interval between scheduled runs of a
	// package; a package's own run_delay overrides it. Default 5m.
	RunDelay string `json:"run_delay"`
	// MaxResets is how many times a run is reset for exceeding run_timeout
	// before it is failed. Default 2. nil means default.
	MaxResets *int `json:"max_resets"`
}

type slackConfig struct {
	DefaultChannels []string            `json:"default_channels"`
	CustomChannels  map[string][]string `json:"custom_channels"`
}
