package setup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/nibzard/ohya/internal/observe"
)

type serviceSetupPhase uint8

const (
	servicePreflight serviceSetupPhase = iota
	serviceStopped
	serviceApplyingUnit
	serviceStarting
)

func configureService(ctx context.Context, o Options, service Service) []Result {
	return configureOwnedService(ctx, o, service, "service", "service.json", true)
}

func configureOwnedService(ctx context.Context, o Options, service Service, component, receiptName string, withRecovery bool) (results []Result) {
	result := Result{Component: component, Status: "failed"}
	receipt := filepath.Join(o.Clients.State, "setup", receiptName)
	if service.StderrPath != "" {
		if err := PrepareStderrLog(service.StderrPath); err != nil {
			result.Detail = err.Error()
			return append(results, result)
		}
	}
	_ = observe.CleanStaleInstances(filepath.Join(o.Clients.State, "observe"), observe.NewSystemClock())
	wasLoaded := o.LoadedService(ctx, service.Loaded) == nil
	original, readErr := readRegular(service.Path)
	owned, receiptErr := loadFileReceipt(receipt)
	var err error
	switch {
	case receiptErr != nil && !os.IsNotExist(receiptErr):
		err = receiptErr
	case wasLoaded && os.IsNotExist(receiptErr):
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
	if err != nil {
		result.Detail = err.Error()
		return append(results, result)
	}

	phase := servicePreflight
	var recovery, migration Result
	defer func() {
		if err != nil {
			result.Detail = err.Error()
			if phase >= serviceStopped {
				recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
				defer cancel()
				detail, restored := rollbackServiceSetup(recoveryCtx, o, service, receipt, original, wasLoaded, phase)
				result.Detail += detail
				if restored {
					result.Changed = false
				}
			}
			if migration.Status == "updated" {
				result.Detail += "; database schema was already migrated"
			}
			if recovery.Status == "saved" {
				result.Detail += "; see the backup result for the recovery point"
			}
		}
		results = append(results, result)
	}()

	if wasLoaded {
		if err = runServiceCommands(ctx, service.Stop, o.RunService); err != nil {
			return results
		}
		phase = serviceStopped
	}
	if withRecovery {
		recovery, err = recoveryPoint(ctx, o)
		if recovery.Component != "" {
			results = append(results, recovery)
		}
		if err != nil {
			return results
		}
		migration, err = migrateDatabase(ctx, o)
		if migration.Component != "" {
			results = append(results, migration)
		}
		if err != nil {
			return results
		}
	}
	phase = serviceApplyingUnit
	result.Changed, err = ApplyFile(service.Path, receipt, service.Content)
	if err != nil {
		return results
	}
	phase = serviceStarting
	if err = runServiceCommands(ctx, service.Start, o.RunService); err != nil {
		return results
	}
	if err = waitService(ctx, service, o.RunService); err != nil {
		return results
	}
	result.Status = "running"
	result.Detail = "History imports in the background; run ohya doctor to check sources."
	if component == "hub-service" {
		result.Detail = "The hub serves devices; run ohya devices invite for another code."
	}
	return results
}

func rollbackServiceSetup(ctx context.Context, o Options, service Service, receipt string, original []byte, wasLoaded bool, phase serviceSetupPhase) (detail string, restored bool) {
	if len(original) == 0 {
		if phase < serviceApplyingUnit {
			return "", true
		}
		if err := cleanupInstalledService(ctx, o, service, receipt); err != nil {
			return "; newly installed service could not be removed: " + err.Error(), false
		}
		return "; newly installed service removed", true
	}
	if wasLoaded {
		if err := restoreService(ctx, o, service, receipt, original); err != nil {
			return "; previous service could not restart: " + err.Error(), false
		}
		return "; previous service restored", true
	}
	if phase >= serviceStarting && o.LoadedService(ctx, service.Loaded) == nil {
		if err := runServiceCommands(ctx, service.Stop, o.RunService); err != nil {
			return "; service could not be stopped: " + err.Error(), false
		}
	}
	if _, err := ApplyFile(service.Path, receipt, original); err != nil {
		return "; previous service configuration could not be restored: " + err.Error(), false
	}
	return "; previous service configuration restored", true
}

func cleanupInstalledService(ctx context.Context, o Options, service Service, receipt string) error {
	if o.LoadedService(ctx, service.Loaded) == nil {
		if err := runServiceCommands(ctx, service.Stop, o.RunService); err != nil {
			return err
		}
	}
	_, err := RemoveFile(receipt)
	return err
}

func restoreService(ctx context.Context, o Options, service Service, receipt string, original []byte) error {
	if _, err := ApplyFile(service.Path, receipt, original); err != nil {
		return err
	}
	if o.LoadedService(ctx, service.Loaded) == nil {
		if err := runServiceCommands(ctx, service.Stop, o.RunService); err != nil {
			return err
		}
	}
	if err := runServiceCommands(ctx, service.Start, o.RunService); err != nil {
		return err
	}
	return waitService(ctx, service, o.RunService)
}
