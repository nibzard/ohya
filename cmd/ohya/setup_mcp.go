package main

import (
	"time"

	"github.com/nibzard/ohya/internal/setup"
)

func setupMCPBuilds(observeDir, revision string, dirty bool) *setup.Result {
	findings := checkMCPBuilds(observeDir, revision, dirty, time.Now().UnixMilli())
	if len(findings) != 1 || findings[0].Severity != sevWarn {
		return nil
	}
	return &setup.Result{Component: "mcp-builds", Status: "warning", Detail: findings[0].Detail + "; " + findings[0].Remedy}
}
