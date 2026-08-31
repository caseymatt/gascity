package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var managedDoltPreflightCleanupFn = preflightManagedDoltCleanup

const (
	managedDoltProcTimeout      = 1500 * time.Millisecond
	managedDoltLsofTimeout      = 3 * time.Second
	managedDoltStaleTempFileAge = 24 * time.Hour
)

var (
	managedDoltProcDir         = "/proc"
	managedDoltUnixSocketTable = "/proc/net/unix"
)

func preflightManagedDoltCleanup(cityPath string) error {
	if err := removeStaleManagedDoltSockets(); err != nil {
		return err
	}
	if err := removeStaleManagedDoltTempFiles(cityPath, time.Now()); err != nil {
		fmt.Fprintf(os.Stderr, "gc: managed dolt stale temp-file cleanup: %v\n", err) //nolint:errcheck // startup must continue
	}
	return nil
}

// removeStaleManagedDoltTempFiles reclaims crash-leaked NBS scratch files
// before the managed server opens any database. Dolt normally removes files
// created by a cleanly exiting process, but a hard stop can leave multi-GB
// buffered_file_byte_sink files in .dolt/temptf indefinitely. The startup
// caller has already proved the data-dir lock is free, so files older than
// Dolt's own 24-hour stale-file threshold cannot belong to the next process.
func removeStaleManagedDoltTempFiles(cityPath string, now time.Time) error {
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		return err
	}
	databases, err := os.ReadDir(layout.DataDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read managed dolt data dir: %w", err)
	}

	var cleanupErr error
	for _, database := range databases {
		if !database.IsDir() || database.Type()&os.ModeSymlink != 0 {
			continue
		}
		tempDir := filepath.Join(layout.DataDir, database.Name(), ".dolt", "temptf")
		err := filepath.Walk(tempDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				if os.IsNotExist(walkErr) {
					return nil
				}
				return walkErr
			}
			if !info.Mode().IsRegular() || now.Sub(info.ModTime()) <= managedDoltStaleTempFileAge {
				return nil
			}
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				return err
			}
			return nil
		})
		if err != nil && !os.IsNotExist(err) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("%s: %w", tempDir, err))
		}
	}
	return cleanupErr
}

var errManagedDoltOpenStateUnknown = errors.New("managed dolt open-file state unknown")

func removeStaleManagedDoltSockets() error {
	for _, path := range staleManagedDoltSocketPaths() {
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSocket == 0 {
			continue
		}
		open, err := fileOpenedByAnyProcess(path)
		if err != nil {
			if errors.Is(err, errManagedDoltOpenStateUnknown) {
				continue
			}
			return err
		}
		if open {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func staleManagedDoltSocketPaths() []string {
	seen := map[string]struct{}{}
	paths := make([]string, 0, 8)
	add := func(path string) {
		if strings.TrimSpace(path) == "" {
			return
		}
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		paths = append(paths, path)
	}
	matches, _ := filepath.Glob("/tmp/dolt*.sock")
	for _, match := range matches {
		add(match)
	}
	return paths
}

func fileOpenedByAnyProcess(path string) (bool, error) {
	if open, checked := unixSocketOpenStateFromTable(path); checked {
		return open, nil
	}
	procCtx, procCancel := context.WithTimeout(context.Background(), managedDoltProcTimeout)
	open, checked := fileOpenedByAnyProcessFromProc(procCtx, path)
	procErr := procCtx.Err()
	procCancel()
	if checked {
		return open, nil
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		if procErr != nil {
			return false, fmt.Errorf("%w: proc probe timed out and lsof unavailable", errManagedDoltOpenStateUnknown)
		}
		return false, errManagedDoltOpenStateUnknown
	}
	lsofCtx, lsofCancel := context.WithTimeout(context.Background(), managedDoltLsofTimeout)
	defer lsofCancel()
	cmd := exec.CommandContext(lsofCtx, "lsof", path)
	cmd.WaitDelay = 100 * time.Millisecond
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	out, err := cmd.CombinedOutput()
	if lsofCtx.Err() != nil {
		return false, fmt.Errorf("%w: lsof probe timed out", errManagedDoltOpenStateUnknown)
	}
	if err == nil {
		return true, nil
	}
	exitErr := &exec.ExitError{}
	if errors.As(err, &exitErr) {
		return false, nil
	}
	return false, fmt.Errorf("lsof %s: %w: %s", path, err, strings.TrimSpace(string(out)))
}

func unixSocketOpenStateFromTable(path string) (bool, bool) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), managedDoltProcTimeout)
	defer cancel()
	inodes, checked := unixSocketInodesForPath(ctx, path)
	if !checked || ctx.Err() != nil {
		return false, false
	}
	return len(inodes) > 0, true
}

func fileOpenedByAnyProcessFromProc(ctx context.Context, path string) (bool, bool) {
	if ctx != nil && ctx.Err() != nil {
		return false, false
	}
	info, statErr := os.Lstat(path)
	isSocketPath := statErr == nil && info.Mode()&os.ModeSocket != 0
	if isSocketPath {
		socketInodes, checked := unixSocketInodesForPath(ctx, path)
		if ctx != nil && ctx.Err() != nil {
			return false, false
		}
		if checked {
			return len(socketInodes) > 0, true
		}
	}
	entries, err := os.ReadDir(managedDoltProcDir)
	if err != nil {
		return false, false
	}
	if ctx != nil && ctx.Err() != nil {
		return false, false
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			return false, false
		}
		if !entry.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		fdDir := filepath.Join(managedDoltProcDir, entry.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			if ctx.Err() != nil {
				return false, false
			}
			target, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			target = strings.TrimSuffix(target, " (deleted)")
			if samePath(target, path) {
				return true, true
			}
		}
	}
	return false, true
}

func unixSocketInodesForPath(ctx context.Context, path string) (map[string]struct{}, bool) {
	if ctx != nil && ctx.Err() != nil {
		return nil, false
	}
	data, err := os.ReadFile(managedDoltUnixSocketTable)
	if err != nil {
		return nil, false
	}
	if ctx != nil && ctx.Err() != nil {
		return nil, false
	}
	inodes := map[string]struct{}{}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	for scanner.Scan() {
		if ctx != nil && ctx.Err() != nil {
			return nil, false
		}
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 {
			continue
		}
		if !samePath(fields[len(fields)-1], path) {
			continue
		}
		inodes[fields[6]] = struct{}{}
	}
	if scanner.Err() != nil {
		return nil, false
	}
	return inodes, true
}
