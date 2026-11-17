package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

func configureService(ctx context.Context, o Options, service Service) []Result {
	return configureOwnedService(ctx, o, service, "service", "service.json", true)
}

func configureOwnedService(ctx context.Context, o Options, service Service, component, receiptName string, withRecovery bool) (results []Result) {
	result := Result{Component: component, Status: "failed"}
	receipt := filepath.Join(o.Clients.State, "setup", receiptName)
	active := o.LoadedService(ctx, service.Loaded) == nil
	original, readErr := readRegular(service.Path)
	owned, receiptErr := loadFileReceipt(receipt)
	var err error
	switch {
	case receiptErr != nil && !os.IsNotExist(receiptErr):
		err = receiptErr
	case active && os.IsNotExist(receiptErr):
		err = errors.New("an unowned ohya service is already loaded")
	case readErr != nil && !os.IsNotExist(readErr):
		err = readErr
	case readErr == nil && os.IsNotExist(receiptErr):
		err = fmt.Errorf("%s is not owned by setup", service.Path)
	case receiptErr == nil && owned.Path != service.Path:
		err = errors.New("setup receipt belongs to another service")
	case readErr == nil && !bytes.Equal(original, owned.Content) && !bytes.Equal(original, owned.Previous):
		err = fmt.Errorf("%s has been edited; preserve it", service.Path)
	}
	stopped := false
	if err == nil && active {
		for _, command := range service.Stop {
			if err = o.RunService(ctx, command); err != nil {
				break
			}
		}
		stopped = err == nil
	}
	if err == nil && withRecovery {
		var recovery Result
		recovery, err = recoveryPoint(ctx, o)
		if recovery.Component != "" {
			results = append(results, recovery)
		}
	}
	if err == nil {
		result.Changed, err = ApplyFile(service.Path, receipt, service.Content)
	}
	if err == nil {
		for _, command := range service.Start {
			if err = o.RunService(ctx, command); err != nil {
				break
			}
		}
	}
	if err == nil {
		err = waitService(ctx, service, o.RunService)
	}
	if err != nil {
		result.Detail = err.Error()
		if stopped && len(original) > 0 {
			recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
			defer cancel()
			if restoreErr := restoreService(recoveryCtx, o, service, receipt, original); restoreErr != nil {
				result.Detail += "; previous service could not restart: " + restoreErr.Error()
			} else {
				result.Detail += "; previous service restored"
			}
		}
	} else {
		result.Status = "running"
		result.Detail = "History imports in the background; run ohya doctor to check sources."
		if component == "hub-service" {
			result.Detail = "The hub serves devices; run ohya devices invite for another code."
		}
	}
	return append(results, result)
}

func restoreService(ctx context.Context, o Options, service Service, receipt string, original []byte) error {
	if _, err := ApplyFile(service.Path, receipt, original); err != nil {
		return err
	}
	if o.LoadedService(ctx, service.Loaded) == nil {
		for _, command := range service.Stop {
			if err := o.RunService(ctx, command); err != nil {
				return err
			}
		}
	}
	for _, command := range service.Start {
		if err := o.RunService(ctx, command); err != nil {
			return err
		}
	}
	return waitService(ctx, service, o.RunService)
}
