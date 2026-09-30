package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: host <module.wasm> [module arguments...]\n")
		os.Exit(2)
	}

	if err := run(context.Background(), os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "host: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, wasmPath string, args []string) error {
	wasm, err := os.ReadFile(wasmPath)
	if err != nil {
		return fmt.Errorf("load module: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[host] loaded %s (%d bytes)\n", wasmPath, len(wasm))

	runtime := wazero.NewRuntime(ctx)
	defer runtime.Close(ctx)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, runtime); err != nil {
		return fmt.Errorf("instantiate WASI: %w", err)
	}

	compiled, err := runtime.CompileModule(ctx, wasm)
	if err != nil {
		return fmt.Errorf("compile module: %w", err)
	}

	fmt.Fprintln(os.Stderr, "[host] compiled module")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	config := wazero.NewModuleConfig().
		WithArgs(append([]string{"guest.wasm"}, args...)...).
		WithStdout(&stdout).
		WithStderr(&stderr)

	instance, execErr := runtime.InstantiateModule(ctx, compiled, config)
	if instance != nil {
		if err := instance.Close(ctx); err != nil {
			return fmt.Errorf("close instance: %w", err)
		}
	}
	fmt.Fprintln(os.Stderr, "[host] instance closed")

	if err := compiled.Close(ctx); err != nil {
		return fmt.Errorf("close compiled module: %w", err)
	}
	fmt.Fprintln(os.Stderr, "[host] compiled module unloaded")

	if stderr.Len() > 0 {
		_, _ = os.Stderr.Write(stderr.Bytes())
	}
	if stdout.Len() > 0 {
		_, _ = os.Stdout.Write(stdout.Bytes())
	}

	var exitErr *sys.ExitError
	if execErr != nil && (!errors.As(execErr, &exitErr) || exitErr.ExitCode() != 0) {
		return fmt.Errorf("execute module: %w", execErr)
	}
	return nil
}
