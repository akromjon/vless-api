package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

type XrayRuntime interface {
	Validate(configPath string) error
	Start() error
	Stop() error
	Restart() error
	IsActive() (bool, error)
	Version() string
}

type CommandRuntime struct {
	XrayBinary      string
	SystemctlBinary string
	ServiceName     string
	Timeout         time.Duration
}

func (r CommandRuntime) Validate(configPath string) error {
	_, err := r.run(r.XrayBinary, "run", "-test", "-config", configPath)
	return err
}

func (r CommandRuntime) Start() error {
	_, err := r.run(r.SystemctlBinary, "start", r.ServiceName)
	return err
}

func (r CommandRuntime) Stop() error {
	_, err := r.run(r.SystemctlBinary, "stop", r.ServiceName)
	return err
}

func (r CommandRuntime) Restart() error {
	_, err := r.run(r.SystemctlBinary, "restart", r.ServiceName)
	return err
}

func (r CommandRuntime) IsActive() (bool, error) {
	_, err := r.run(r.SystemctlBinary, "is-active", "--quiet", r.ServiceName)
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 3 {
		return false, nil
	}
	return false, err
}

func (r CommandRuntime) Version() string {
	output, err := r.run(r.XrayBinary, "version")
	if err != nil {
		return "unknown"
	}
	line, _, _ := strings.Cut(strings.TrimSpace(output), "\n")
	return line
}

func (r CommandRuntime) run(name string, args ...string) (string, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	command := exec.CommandContext(ctx, name, args...)
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return output.String(), fmt.Errorf("%s timed out after %s", name, timeout)
		}
		message := strings.TrimSpace(output.String())
		if message == "" {
			message = err.Error()
		}
		return output.String(), fmt.Errorf("%s failed: %s: %w", name, message, err)
	}
	return output.String(), nil
}
