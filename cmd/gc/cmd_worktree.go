package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/spf13/cobra"
)

const worktreeRegistryVersion = 1

var (
	worktreeNow         = time.Now
	worktreeAtomicWrite = func(path string, data []byte, perm fs.FileMode) error {
		return fsys.WriteFileAtomic(fsys.OSFS{}, path, data, perm)
	}
)

type worktreeRegistry struct {
	Version int                     `json:"version"`
	Entries []worktreeRegistryEntry `json:"entries"`
}

type worktreeRegistryEntry struct {
	ID             string `json:"id"`
	Owner          string `json:"owner"`
	Rig            string `json:"rig"`
	RigRoot        string `json:"rig_root"`
	Path           string `json:"path"`
	Attempt        int    `json:"attempt"`
	Base           string `json:"base"`
	Branch         string `json:"branch"`
	HeadSHA        string `json:"head_sha"`
	CreatedAt      string `json:"created_at"`
	CargoTargetDir string `json:"cargo_target_dir"`
	CargoHome      string `json:"cargo_home"`
	Published      bool   `json:"published"`
	PublishedRef   string `json:"published_ref"`
	PublishedSHA   string `json:"published_sha"`
	PublishedAt    string `json:"published_at"`
}

type worktreeListEntry struct {
	worktreeRegistryEntry
	SizeBytes   int64  `json:"size_bytes"`
	Reclaimable bool   `json:"reclaimable"`
	Reason      string `json:"reason"`
}

type worktreeReclaimResult struct {
	ID           string `json:"id"`
	Owner        string `json:"owner"`
	Rig          string `json:"rig"`
	Path         string `json:"path"`
	Reclaimed    bool   `json:"reclaimed"`
	DryRun       bool   `json:"dry_run"`
	Reclaimable  bool   `json:"reclaimable"`
	Reason       string `json:"reason"`
	HeadSHA      string `json:"head_sha"`
	PublishedRef string `json:"published_ref"`
	PublishedSHA string `json:"published_sha"`
}

type worktreeRig struct {
	Name string
	Root string
}

type worktreeCreateOptions struct {
	ID      string
	Owner   string
	Path    string
	Base    string
	Branch  string
	Attempt int
}

type worktreeRegistryPaths struct {
	Dir      string
	Registry string
	Lock     string
}

func newWorktreeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "worktree",
		Short: "Create, publish, inspect, and safely reclaim managed worktrees",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			fmt.Fprintf(stderr, "gc worktree: unknown subcommand %q\n", args[0]) //nolint:errcheck
			return errExit
		},
	}
	cmd.AddCommand(
		newWorktreeCreateCmd(stdout, stderr),
		newWorktreePublishCmd(stdout, stderr),
		newWorktreeReclaimCmd(stdout, stderr),
		newWorktreeListCmd(stdout, stderr),
	)
	addWorktreeOwnershipCommands(cmd, stdout, stderr)
	return cmd
}

func newWorktreeCreateCmd(stdout, stderr io.Writer) *cobra.Command {
	var opts worktreeCreateOptions
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create <id>",
		Short: "Create and register a managed worktree",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.ID = args[0]
			if flag := cmd.Flags().Lookup("rig"); flag == nil || !flag.Changed {
				err := errors.New("--rig is required")
				fmt.Fprintf(stderr, "gc worktree create: %v\n", err) //nolint:errcheck
				return err
			}
			cityPath, cfg, err := resolveWorktreeCity(jsonOutput, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree create: %v\n", err) //nolint:errcheck
				return err
			}
			rig, err := selectWorktreeRig(cfg, cityPath, rigFlag)
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree create: %v\n", err) //nolint:errcheck
				return err
			}
			entry, err := createRegisteredWorktree(cmd.Context(), cityPath, rig, opts, openCityRecorderAt(cityPath, stderr))
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree create: %v\n", err) //nolint:errcheck
				return err
			}
			view := describeWorktreeEntryForOutput(cmd.Context(), cityPath, cfg, entry)
			if err := writeWorktreeEntryOutput(stdout, view, jsonOutput); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&opts.Owner, "owner", "", "logical lifecycle owner")
	cmd.Flags().StringVar(&opts.Path, "path", "", "checkout path beneath the rig worktrees directory")
	cmd.Flags().StringVar(&opts.Base, "base", "", "base revision for the checkout")
	cmd.Flags().StringVar(&opts.Branch, "branch", "", "new branch name (default: detached HEAD)")
	cmd.Flags().IntVar(&opts.Attempt, "attempt", 1, "positive lifecycle attempt number")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newWorktreePublishCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "publish <id-or-path>",
		Short: "Publish the current worktree HEAD through Code Storage",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cityPath, cfg, err := resolveWorktreeCity(jsonOutput, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree publish: %v\n", err) //nolint:errcheck
				return err
			}
			entry, err := publishRegisteredWorktree(cmd.Context(), cityPath, args[0], openCityRecorderAt(cityPath, stderr))
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree publish: %v\n", err) //nolint:errcheck
				return err
			}
			view := describeWorktreeEntryForOutput(cmd.Context(), cityPath, cfg, entry)
			if err := writeWorktreeEntryOutput(stdout, view, jsonOutput); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newWorktreeReclaimCmd(stdout, stderr io.Writer) *cobra.Command {
	var promotedSHA string
	var dryRun bool
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "reclaim <id-or-path>",
		Short: "Safely remove a published or promoted managed worktree",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cityPath, cfg, err := resolveWorktreeCity(jsonOutput, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree reclaim: %v\n", err) //nolint:errcheck
				return err
			}
			dryRun = dryRun || os.Getenv("GC_WORKTREE_CLEANUP_DRY_RUN") == "1"
			result, err := reclaimRegisteredWorktree(cmd.Context(), cityPath, cfg, args[0], strings.TrimSpace(promotedSHA), dryRun, openCityRecorderAt(cityPath, stderr))
			if err == nil || result.ID != "" {
				if writeErr := writeWorktreeReclaimOutput(stdout, result, jsonOutput); writeErr != nil {
					return fmt.Errorf("writing output: %w", writeErr)
				}
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree reclaim: %v\n", err) //nolint:errcheck
				return err
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&promotedSHA, "promoted-sha", "", "trusted promoted commit that must descend from the worktree HEAD")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report an eligible reclaim without removing anything")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func newWorktreeListCmd(stdout, stderr io.Writer) *cobra.Command {
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "list [id-or-path]",
		Short: "List managed worktrees and their reclaim status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cityPath, cfg, err := resolveWorktreeCity(jsonOutput, stderr)
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree list: %v\n", err) //nolint:errcheck
				return err
			}
			selector := ""
			if len(args) == 1 {
				selector = args[0]
			}
			rigFilter := ""
			if flag := cmd.Flags().Lookup("rig"); flag != nil && flag.Changed {
				rigFilter = rigFlag
			}
			entries, err := listRegisteredWorktrees(cmd.Context(), cityPath, cfg, selector, rigFilter)
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree list: %v\n", err) //nolint:errcheck
				return err
			}
			if err := writeWorktreeListOutput(stdout, entries, jsonOutput); err != nil {
				return fmt.Errorf("writing output: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	return cmd
}

func resolveWorktreeCity(jsonOutput bool, stderr io.Writer) (string, *config.City, error) {
	resolved, err := resolveContext()
	if err != nil {
		return "", nil, err
	}
	warningWriter := stderr
	if jsonOutput {
		warningWriter = io.Discard
	}
	cfg, err := loadCityConfig(resolved.CityPath, warningWriter)
	if err != nil {
		return "", nil, fmt.Errorf("loading city config: %w", err)
	}
	resolveRigPaths(resolved.CityPath, cfg.Rigs)
	return pathutil.NormalizePathForCompare(resolved.CityPath), cfg, nil
}

func selectWorktreeRig(cfg *config.City, cityPath, selector string) (worktreeRig, error) {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return worktreeRig{}, errors.New("--rig is required")
	}
	if cfg == nil {
		return worktreeRig{}, errors.New("city config is unavailable")
	}
	for i := range cfg.Rigs {
		candidate := cfg.Rigs[i]
		root := candidate.Path
		if !filepath.IsAbs(root) {
			root = filepath.Join(cityPath, root)
		}
		root = pathutil.NormalizePathForCompare(root)
		if strings.EqualFold(candidate.Name, selector) || pathutil.SamePath(root, selector) {
			if root == "" {
				return worktreeRig{}, fmt.Errorf("rig %q has no canonical root", candidate.Name)
			}
			if info, err := os.Stat(root); err != nil {
				return worktreeRig{}, fmt.Errorf("stat rig %q root: %w", candidate.Name, err)
			} else if !info.IsDir() {
				return worktreeRig{}, fmt.Errorf("rig %q root is not a directory", candidate.Name)
			}
			if !git.New(root).IsRepoCtx(context.Background()) {
				return worktreeRig{}, fmt.Errorf("rig %q root is not a git repository", candidate.Name)
			}
			return worktreeRig{Name: candidate.Name, Root: root}, nil
		}
	}
	return worktreeRig{}, fmt.Errorf("unknown rig %q", selector)
}

func worktreeRegistryFilePaths(cityPath string) worktreeRegistryPaths {
	dir := filepath.Join(cityPath, ".gc", "worktrees")
	return worktreeRegistryPaths{
		Dir:      dir,
		Registry: filepath.Join(dir, "registry.json"),
		Lock:     filepath.Join(dir, "registry.lock"),
	}
}

func withWorktreeRegistry(cityPath string, fn func(worktreeRegistryPaths, *worktreeRegistry) error) (returnErr error) {
	cityPath = pathutil.NormalizePathForCompare(cityPath)
	if cityPath == "" {
		return errors.New("city path is empty")
	}
	paths := worktreeRegistryFilePaths(cityPath)
	if err := os.MkdirAll(paths.Dir, 0o755); err != nil {
		return fmt.Errorf("creating worktree registry directory: %w", err)
	}
	if !pathutil.PathWithin(cityPath, paths.Dir) {
		return fmt.Errorf("worktree registry directory escapes city root: %s", paths.Dir)
	}
	if info, err := os.Lstat(paths.Lock); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("worktree registry lock is not a regular file: %s", paths.Lock)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspecting worktree registry lock: %w", err)
	}
	lock := beads.NewFileFlock(paths.Lock)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("locking worktree registry: %w", err)
	}
	defer func() {
		if err := lock.Unlock(); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("unlocking worktree registry: %w", err))
		}
	}()
	registry, err := loadWorktreeRegistry(paths.Registry)
	if err != nil {
		return err
	}
	return fn(paths, registry)
}

func loadWorktreeRegistry(path string) (*worktreeRegistry, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &worktreeRegistry{Version: worktreeRegistryVersion, Entries: []worktreeRegistryEntry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading worktree registry: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var registry worktreeRegistry
	if err := dec.Decode(&registry); err != nil {
		return nil, fmt.Errorf("malformed worktree registry: %w", err)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return nil, errors.New("malformed worktree registry: trailing JSON data")
	}
	if registry.Version != worktreeRegistryVersion {
		return nil, fmt.Errorf("unsupported worktree registry version %d", registry.Version)
	}
	if registry.Entries == nil {
		registry.Entries = []worktreeRegistryEntry{}
	}
	if err := validateWorktreeRegistry(&registry); err != nil {
		return nil, fmt.Errorf("malformed worktree registry: %w", err)
	}
	return &registry, nil
}

func validateWorktreeRegistry(registry *worktreeRegistry) error {
	ids := make(map[string]struct{}, len(registry.Entries))
	paths := make(map[string]string, len(registry.Entries))
	for i := range registry.Entries {
		entry := registry.Entries[i]
		if err := validateWorktreeRegistryEntry(entry); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}
		if _, exists := ids[entry.ID]; exists {
			return fmt.Errorf("duplicate id %q", entry.ID)
		}
		ids[entry.ID] = struct{}{}
		normalizedPath := pathutil.NormalizePathForCompare(entry.Path)
		if priorID, exists := paths[normalizedPath]; exists {
			return fmt.Errorf("duplicate path %q for ids %q and %q", entry.Path, priorID, entry.ID)
		}
		paths[normalizedPath] = entry.ID
	}
	return nil
}

func validateWorktreeRegistryEntry(entry worktreeRegistryEntry) error {
	if err := validateWorktreeID(entry.ID); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"owner": entry.Owner, "rig": entry.Rig, "rig_root": entry.RigRoot,
		"path": entry.Path, "base": entry.Base, "head_sha": entry.HeadSHA,
		"created_at": entry.CreatedAt, "cargo_target_dir": entry.CargoTargetDir,
		"cargo_home": entry.CargoHome,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is empty", name)
		}
	}
	if entry.Attempt < 1 {
		return fmt.Errorf("attempt %d is not positive", entry.Attempt)
	}
	if pathutil.NormalizePathForCompare(entry.RigRoot) != entry.RigRoot {
		return errors.New("rig_root is not canonical")
	}
	if pathutil.NormalizePathForCompare(entry.Path) != entry.Path {
		return errors.New("path is not canonical")
	}
	worktreesRoot := filepath.Join(entry.RigRoot, "worktrees")
	if !pathutil.PathWithin(worktreesRoot, entry.Path) || pathutil.SamePath(worktreesRoot, entry.Path) {
		return errors.New("path is not strictly beneath the rig worktrees directory")
	}
	wantTarget := filepath.Join(worktreesRoot, ".cargo-targets", entry.ID, "attempt-"+strconv.Itoa(entry.Attempt))
	if !pathutil.SamePath(wantTarget, entry.CargoTargetDir) {
		return errors.New("cargo_target_dir does not match id and attempt")
	}
	wantHome := filepath.Join(entry.RigRoot, ".gc", "cache", "cargo-home")
	if !pathutil.SamePath(wantHome, entry.CargoHome) {
		return errors.New("cargo_home does not match rig root")
	}
	if _, err := time.Parse(time.RFC3339Nano, entry.CreatedAt); err != nil {
		return fmt.Errorf("invalid created_at: %w", err)
	}
	if entry.Published {
		if strings.TrimSpace(entry.PublishedRef) == "" || strings.TrimSpace(entry.PublishedSHA) == "" || strings.TrimSpace(entry.PublishedAt) == "" {
			return errors.New("published entry has incomplete publication fields")
		}
		if _, err := time.Parse(time.RFC3339Nano, entry.PublishedAt); err != nil {
			return fmt.Errorf("invalid published_at: %w", err)
		}
	} else if entry.PublishedRef != "" || entry.PublishedSHA != "" || entry.PublishedAt != "" {
		return errors.New("unpublished entry has publication fields")
	}
	return nil
}

func validateWorktreeID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("id is empty")
	}
	if id != filepath.Base(id) || id == "." || id == ".." || strings.ContainsAny(id, `/\\`) {
		return fmt.Errorf("id %q must be one path component", id)
	}
	return nil
}

func writeWorktreeRegistry(path string, registry *worktreeRegistry) error {
	if err := validateWorktreeRegistry(registry); err != nil {
		return fmt.Errorf("refusing to write invalid worktree registry: %w", err)
	}
	sort.Slice(registry.Entries, func(i, j int) bool { return registry.Entries[i].ID < registry.Entries[j].ID })
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding worktree registry: %w", err)
	}
	data = append(data, '\n')
	if err := worktreeAtomicWrite(path, data, 0o644); err != nil {
		return fmt.Errorf("writing worktree registry: %w", err)
	}
	return nil
}

func createRegisteredWorktree(ctx context.Context, cityPath string, rig worktreeRig, opts worktreeCreateOptions, rec events.Recorder) (worktreeRegistryEntry, error) {
	var result worktreeRegistryEntry
	if err := validateWorktreeCreateOptions(opts); err != nil {
		return result, err
	}
	rig.Root = pathutil.NormalizePathForCompare(rig.Root)
	if rig.Name == "" || rig.Root == "" {
		return result, errors.New("selected rig is incomplete")
	}
	worktreesRoot := filepath.Join(rig.Root, "worktrees")
	if err := os.MkdirAll(worktreesRoot, 0o755); err != nil {
		return result, fmt.Errorf("creating rig worktrees directory: %w", err)
	}
	worktreesRoot = pathutil.NormalizePathForCompare(worktreesRoot)
	requestedPath := opts.Path
	if !filepath.IsAbs(requestedPath) {
		requestedPath = filepath.Join(worktreesRoot, requestedPath)
	}
	requestedPath = pathutil.NormalizePathForCompare(requestedPath)
	if !pathutil.PathWithin(worktreesRoot, requestedPath) || pathutil.SamePath(worktreesRoot, requestedPath) {
		return result, fmt.Errorf("path %q must be strictly beneath %s", opts.Path, worktreesRoot)
	}
	branch := strings.TrimSpace(opts.Branch)
	if branch == "" {
		branch = "HEAD"
	}
	result = worktreeRegistryEntry{
		ID:             opts.ID,
		Owner:          strings.TrimSpace(opts.Owner),
		Rig:            rig.Name,
		RigRoot:        rig.Root,
		Path:           requestedPath,
		Attempt:        opts.Attempt,
		Base:           strings.TrimSpace(opts.Base),
		Branch:         branch,
		CargoTargetDir: filepath.Join(worktreesRoot, ".cargo-targets", opts.ID, "attempt-"+strconv.Itoa(opts.Attempt)),
		CargoHome:      filepath.Join(rig.Root, ".gc", "cache", "cargo-home"),
		Published:      false,
	}

	err := withWorktreeRegistry(cityPath, func(paths worktreeRegistryPaths, registry *worktreeRegistry) error {
		for i := range registry.Entries {
			existing := registry.Entries[i]
			if existing.ID == result.ID {
				if !sameWorktreeCreateRequest(existing, result) {
					return fmt.Errorf("id %q is already registered with different create parameters", result.ID)
				}
				owned, err := rigOwnsWorktreePath(rig.Root, existing.Path)
				if err != nil {
					return fmt.Errorf("verifying idempotent worktree: %w", err)
				}
				if !owned {
					return fmt.Errorf("id %q is registered but its checkout is not owned by rig %q", result.ID, rig.Name)
				}
				if err := os.MkdirAll(existing.CargoTargetDir, 0o755); err != nil {
					return fmt.Errorf("restoring cargo target directory: %w", err)
				}
				if err := os.MkdirAll(existing.CargoHome, 0o755); err != nil {
					return fmt.Errorf("restoring cargo home: %w", err)
				}
				result = existing
				return nil
			}
			if pathutil.SamePath(existing.Path, result.Path) {
				return fmt.Errorf("path %q is already registered to id %q", result.Path, existing.ID)
			}
		}
		if registered, err := rigOwnsWorktreePath(rig.Root, result.Path); err != nil {
			return fmt.Errorf("checking existing rig worktrees: %w", err)
		} else if registered {
			return fmt.Errorf("path %q is already an unregistered worktree", result.Path)
		}
		if _, err := os.Lstat(result.Path); err == nil {
			return fmt.Errorf("path %q already exists", result.Path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspecting worktree path: %w", err)
		}

		targetExisted := pathExists(result.CargoTargetDir)
		if err := os.MkdirAll(result.CargoTargetDir, 0o755); err != nil {
			return fmt.Errorf("creating cargo target directory: %w", err)
		}
		if err := os.MkdirAll(result.CargoHome, 0o755); err != nil {
			if !targetExisted {
				_ = os.Remove(result.CargoTargetDir)
			}
			return fmt.Errorf("creating cargo home: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(result.Path), 0o755); err != nil {
			if !targetExisted {
				_ = os.Remove(result.CargoTargetDir)
			}
			return fmt.Errorf("creating worktree parent: %w", err)
		}
		addBranch := strings.TrimSpace(opts.Branch)
		if err := git.New(rig.Root).WorktreeAddCtx(ctx, result.Path, result.Base, addBranch); err != nil {
			if !targetExisted {
				_ = os.Remove(result.CargoTargetDir)
			}
			return err
		}
		compensate := func(cause error) error {
			removeErr := git.New(rig.Root).WorktreeRemove(result.Path, false)
			if !targetExisted {
				_ = os.Remove(result.CargoTargetDir)
			}
			if removeErr != nil {
				return errors.Join(cause, fmt.Errorf("compensating worktree removal: %w", removeErr))
			}
			return cause
		}
		actualBranch, err := git.New(result.Path).CurrentBranchCtx(ctx)
		if err != nil {
			return compensate(err)
		}
		if actualBranch != result.Branch {
			return compensate(fmt.Errorf("created branch %q does not match requested branch %q", actualBranch, result.Branch))
		}
		head, err := git.New(result.Path).HeadCtx(ctx)
		if err != nil {
			return compensate(err)
		}
		result.HeadSHA = head
		result.CreatedAt = worktreeNow().UTC().Format(time.RFC3339Nano)
		if err := invokeCodeStorageAttach(ctx, cityPath, result); err != nil {
			return compensate(err)
		}
		registry.Entries = append(registry.Entries, result)
		if err := writeWorktreeRegistry(paths.Registry, registry); err != nil {
			registry.Entries = registry.Entries[:len(registry.Entries)-1]
			return compensate(err)
		}
		recordWorktreeCreated(rec, result)
		return nil
	})
	return result, err
}

func validateWorktreeCreateOptions(opts worktreeCreateOptions) error {
	if err := validateWorktreeID(opts.ID); err != nil {
		return err
	}
	if strings.TrimSpace(opts.Owner) == "" {
		return errors.New("--owner is required")
	}
	if strings.TrimSpace(opts.Path) == "" {
		return errors.New("--path is required")
	}
	if strings.TrimSpace(opts.Base) == "" {
		return errors.New("--base is required")
	}
	if opts.Attempt < 1 {
		return errors.New("--attempt must be positive")
	}
	return nil
}

func sameWorktreeCreateRequest(a, b worktreeRegistryEntry) bool {
	return a.ID == b.ID && a.Owner == b.Owner && a.Rig == b.Rig &&
		pathutil.SamePath(a.RigRoot, b.RigRoot) && pathutil.SamePath(a.Path, b.Path) &&
		a.Attempt == b.Attempt && a.Base == b.Base && a.Branch == b.Branch &&
		pathutil.SamePath(a.CargoTargetDir, b.CargoTargetDir) && pathutil.SamePath(a.CargoHome, b.CargoHome)
}

func pathExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func rigOwnsWorktreePath(rigRoot, path string) (bool, error) {
	worktrees, err := git.New(rigRoot).WorktreeList()
	if err != nil {
		return false, err
	}
	for i := range worktrees {
		if pathutil.SamePath(worktrees[i].Path, path) {
			return true, nil
		}
	}
	return false, nil
}

type codeStoragePublishReply struct {
	Worktree        string `json:"worktree"`
	Ref             string `json:"ref"`
	HeadSHA         string `json:"headSha"`
	Pushed          bool   `json:"pushed"`
	AlreadyUpToDate bool   `json:"alreadyUpToDate"`
}

func codeStorageHelperPath(cityPath string) string {
	if configured := strings.TrimSpace(os.Getenv("GC_CODE_STORAGE_HELPER")); configured != "" {
		return configured
	}
	return filepath.Join(cityPath, "tools", "code-storage", "gc-code-storage")
}

func invokeCodeStorageAttach(ctx context.Context, cityPath string, entry worktreeRegistryEntry) error {
	cmd := exec.CommandContext(ctx, codeStorageHelperPath(cityPath), "attach", entry.Rig, entry.ID, strconv.Itoa(entry.Attempt), entry.Path)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return redactedHelperError("attach", err)
	}
	return nil
}

func invokeCodeStoragePublish(ctx context.Context, cityPath string, entry worktreeRegistryEntry) (codeStoragePublishReply, error) {
	var stdout bytes.Buffer
	cmd := exec.CommandContext(ctx, codeStorageHelperPath(cityPath), "publish", entry.Rig, entry.ID, strconv.Itoa(entry.Attempt), entry.Path)
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return codeStoragePublishReply{}, redactedHelperError("publish", err)
	}
	dec := json.NewDecoder(&stdout)
	dec.DisallowUnknownFields()
	var reply codeStoragePublishReply
	if err := dec.Decode(&reply); err != nil {
		return codeStoragePublishReply{}, errors.New("code storage helper publish returned invalid JSON (output redacted)")
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return codeStoragePublishReply{}, errors.New("code storage helper publish returned trailing data (output redacted)")
	}
	reply.Ref = strings.TrimSpace(reply.Ref)
	reply.HeadSHA = strings.TrimSpace(reply.HeadSHA)
	if reply.Worktree != entry.Path {
		return codeStoragePublishReply{}, errors.New("code storage helper publish returned a mismatched worktree (output redacted)")
	}
	if !reply.Pushed {
		return codeStoragePublishReply{}, errors.New("code storage helper publish refused to push (output redacted)")
	}
	if reply.Ref == "" || reply.HeadSHA == "" {
		return codeStoragePublishReply{}, errors.New("code storage helper publish returned an incomplete result (output redacted)")
	}
	return reply, nil
}

func redactedHelperError(verb string, err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return fmt.Errorf("code storage helper %s failed with exit code %d (output redacted)", verb, exitErr.ExitCode())
	}
	return fmt.Errorf("code storage helper %s could not start (details redacted)", verb)
}

func publishRegisteredWorktree(ctx context.Context, cityPath, selector string, rec events.Recorder) (worktreeRegistryEntry, error) {
	var result worktreeRegistryEntry
	err := withWorktreeRegistry(cityPath, func(paths worktreeRegistryPaths, registry *worktreeRegistry) error {
		index, ok := findWorktreeRegistryEntry(registry, selector)
		if !ok {
			return fmt.Errorf("no registered worktree matches %q", selector)
		}
		entry := registry.Entries[index]
		owned, err := rigOwnsWorktreePath(entry.RigRoot, entry.Path)
		if err != nil {
			return fmt.Errorf("verifying worktree ownership: %w", err)
		}
		if !owned {
			return fmt.Errorf("registered path %q is not owned by rig %q", entry.Path, entry.Rig)
		}
		reply, err := invokeCodeStoragePublish(ctx, cityPath, entry)
		if err != nil {
			return err
		}
		currentHead, err := git.New(entry.Path).HeadCtx(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(reply.HeadSHA, currentHead) {
			return errors.New("code storage helper published SHA does not match current HEAD (helper output redacted)")
		}
		entry.HeadSHA = currentHead
		entry.Published = true
		entry.PublishedRef = reply.Ref
		entry.PublishedSHA = reply.HeadSHA
		entry.PublishedAt = worktreeNow().UTC().Format(time.RFC3339Nano)
		registry.Entries[index] = entry
		if err := writeWorktreeRegistry(paths.Registry, registry); err != nil {
			return err
		}
		result = entry
		recordWorktreePublished(rec, entry)
		return nil
	})
	return result, err
}

func findWorktreeRegistryEntry(registry *worktreeRegistry, selector string) (int, bool) {
	selector = strings.TrimSpace(selector)
	for i := range registry.Entries {
		if registry.Entries[i].ID == selector {
			return i, true
		}
	}
	if selector == "" {
		return 0, false
	}
	selectorPath := pathutil.NormalizePathForCompare(selector)
	for i := range registry.Entries {
		if pathutil.SamePath(registry.Entries[i].Path, selectorPath) {
			return i, true
		}
	}
	return 0, false
}

type worktreeSafetySnapshot struct {
	Live      liveWorktreeState
	Worktrees map[string]worktreeLiveness
	Err       error
}

func inspectWorktreeSafety(rigRoot string, live liveWorktreeState) worktreeSafetySnapshot {
	result := worktreeSafetySnapshot{Live: live, Worktrees: make(map[string]worktreeLiveness)}
	if !live.scanned {
		return result
	}
	worktrees, err := discoverWorktreeLiveness(rigRoot, live, nil)
	if err != nil {
		result.Err = err
		return result
	}
	for i := range worktrees {
		result.Worktrees[pathutil.NormalizePathForCompare(worktrees[i].Path)] = worktrees[i]
	}
	return result
}

func classifyWorktreeReclaim(ctx context.Context, cfg *config.City, cityPath string, entry worktreeRegistryEntry, promotedSHA string, safety worktreeSafetySnapshot) (head, reason string) {
	rig, err := selectWorktreeRig(cfg, cityPath, entry.Rig)
	if err != nil {
		return "", fmt.Sprintf("configured rig probe failed (failing closed): %v", err)
	}
	if !pathutil.SamePath(rig.Root, entry.RigRoot) {
		return "", "registered rig root no longer matches city configuration"
	}
	worktreesRoot := filepath.Join(rig.Root, "worktrees")
	if !pathutil.PathWithin(worktreesRoot, entry.Path) || pathutil.SamePath(worktreesRoot, entry.Path) {
		return "", "registered path is outside the rig worktrees directory"
	}
	if !safety.Live.scanned {
		return "", "liveness scan unavailable (failing closed)"
	}
	if safety.Err != nil {
		return "", fmt.Sprintf("worktree ownership probe failed (failing closed): %v", safety.Err)
	}
	liveness, owned := safety.Worktrees[pathutil.NormalizePathForCompare(entry.Path)]
	if !owned || pathutil.SamePath(entry.Path, rig.Root) {
		return "", fmt.Sprintf("path is not a registered worktree owned by rig %s", entry.Rig)
	}
	if liveness.Live {
		return "", "live: " + liveness.Reason
	}
	worktreeGit := git.New(entry.Path)
	if worktreeGit.HasUncommittedWork() {
		return "", "uncommitted work present or git status probe failed (failing closed)"
	}
	hasStashes, err := worktreeGit.HasWorktreeStashesCtx(ctx)
	if err != nil {
		return "", fmt.Sprintf("stash probe failed (failing closed): %v", err)
	}
	if hasStashes {
		return "", "worktree has stashed changes"
	}
	head, err = worktreeGit.HeadCtx(ctx)
	if err != nil {
		return "", fmt.Sprintf("HEAD probe failed (failing closed): %v", err)
	}
	if entry.Published && entry.PublishedSHA != "" && strings.EqualFold(entry.PublishedSHA, head) {
		return head, ""
	}
	if promotedSHA == "" {
		if !entry.Published {
			return head, "current HEAD has no verified publication and no promoted SHA was supplied"
		}
		return head, "current HEAD does not equal the verified published SHA and no promoted SHA was supplied"
	}
	ancestor, err := worktreeGit.IsAncestorCtx(ctx, head, promotedSHA)
	if err != nil {
		return head, fmt.Sprintf("promoted ancestry probe failed (failing closed): %v", err)
	}
	if !ancestor {
		return head, "current HEAD is not an ancestor of the supplied promoted SHA"
	}
	return head, ""
}

func worktreeReclaimCachePaths(entry worktreeRegistryEntry) (string, string, error) {
	cacheRoot := filepath.Join(entry.RigRoot, "worktrees", ".cargo-targets")
	idParent := filepath.Join(cacheRoot, entry.ID)
	expected := filepath.Join(idParent, "attempt-"+strconv.Itoa(entry.Attempt))
	if entry.CargoTargetDir != expected {
		return "", "", errors.New("registered cargo target directory is not the exact per-attempt cache path")
	}
	resolved, err := filepath.EvalSymlinks(expected)
	if err != nil {
		return "", "", fmt.Errorf("resolving registered cargo target directory: %w", err)
	}
	if pathutil.NormalizePathForCompare(resolved) != expected ||
		!pathutil.PathWithin(cacheRoot, resolved) || pathutil.SamePath(cacheRoot, resolved) {
		return "", "", errors.New("registered cargo target directory contains a symlink or escapes the cache root")
	}
	return expected, idParent, nil
}

func removeWorktreeAttemptCache(entry worktreeRegistryEntry) error {
	cachePath, idParent, err := worktreeReclaimCachePaths(entry)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(cachePath); err != nil {
		return fmt.Errorf("removing per-attempt cargo target directory: %w", err)
	}
	if err := os.Remove(idParent); err != nil &&
		!errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
		return fmt.Errorf("removing empty cargo target parent: %w", err)
	}
	return nil
}

func reclaimRegisteredWorktree(ctx context.Context, cityPath string, cfg *config.City, selector, promotedSHA string, dryRun bool, rec events.Recorder) (worktreeReclaimResult, error) {
	var result worktreeReclaimResult
	err := withWorktreeRegistry(cityPath, func(paths worktreeRegistryPaths, registry *worktreeRegistry) error {
		index, ok := findWorktreeRegistryEntry(registry, selector)
		if !ok {
			return fmt.Errorf("no registered worktree matches %q", selector)
		}
		entry := registry.Entries[index]
		result = worktreeReclaimResult{
			ID: entry.ID, Owner: entry.Owner, Rig: entry.Rig, Path: entry.Path,
			DryRun: dryRun, PublishedRef: entry.PublishedRef, PublishedSHA: entry.PublishedSHA,
		}
		live := collectLiveWorktreeStateFn()
		safety := inspectWorktreeSafety(entry.RigRoot, live)
		head, reason := classifyWorktreeReclaim(ctx, cfg, cityPath, entry, promotedSHA, safety)
		result.HeadSHA = head
		if reason != "" {
			result.Reason = reason
			recordWorktreeReclaimSkipped(rec, entry, head, reason, dryRun)
			return errors.New(reason)
		}
		result.Reclaimable = true
		if dryRun {
			result.Reason = "dry-run: would reclaim"
			recordWorktreeReclaimSkipped(rec, entry, head, result.Reason, true)
			return nil
		}
		if err := removeWorktreeAttemptCache(entry); err != nil {
			result.Reclaimable = false
			result.Reason = fmt.Sprintf("cargo target cleanup failed (checkout preserved): %v", err)
			recordWorktreeReclaimSkipped(rec, entry, head, result.Reason, false)
			return errors.New(result.Reason)
		}
		if err := git.New(entry.RigRoot).WorktreeRemove(entry.Path, false); err != nil {
			result.Reclaimable = false
			result.Reason = fmt.Sprintf("git worktree removal failed (checkout preserved when possible): %v", err)
			recordWorktreeReclaimSkipped(rec, entry, head, result.Reason, false)
			return errors.New(result.Reason)
		}
		registry.Entries = append(registry.Entries[:index], registry.Entries[index+1:]...)
		if err := writeWorktreeRegistry(paths.Registry, registry); err != nil {
			recoveryErr := git.New(entry.RigRoot).WorktreeAddCtx(ctx, entry.Path, head, "")
			if recoveryErr == nil {
				recoveryErr = invokeCodeStorageAttach(ctx, cityPath, entry)
			}
			result.Reclaimable = false
			result.Reason = "registry update failed after git removal; checkout recovery attempted"
			if recoveryErr != nil {
				result.Reason += ": " + recoveryErr.Error()
			}
			recordWorktreeReclaimSkipped(rec, entry, head, result.Reason, false)
			return errors.Join(err, errors.New(result.Reason))
		}
		result.Reclaimed = true
		result.Reason = ""
		recordWorktreeReclaimed(rec, entry, head, false)
		return nil
	})
	return result, err
}

func listRegisteredWorktrees(ctx context.Context, cityPath string, cfg *config.City, selector, rigFilter string) ([]worktreeListEntry, error) {
	var entries []worktreeRegistryEntry
	if err := withWorktreeRegistry(cityPath, func(_ worktreeRegistryPaths, registry *worktreeRegistry) error {
		for i := range registry.Entries {
			entry := registry.Entries[i]
			if selector != "" {
				index, ok := findWorktreeRegistryEntry(registry, selector)
				if !ok || index != i {
					continue
				}
			}
			if rigFilter != "" && !strings.EqualFold(entry.Rig, rigFilter) && !pathutil.SamePath(entry.RigRoot, rigFilter) {
				continue
			}
			entries = append(entries, entry)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	live := collectLiveWorktreeStateFn()
	safetyByRig := make(map[string]worktreeSafetySnapshot)
	result := make([]worktreeListEntry, 0, len(entries))
	for i := range entries {
		entry := entries[i]
		safety, ok := safetyByRig[entry.RigRoot]
		if !ok {
			safety = inspectWorktreeSafety(entry.RigRoot, live)
			safetyByRig[entry.RigRoot] = safety
		}
		head, reason := classifyWorktreeReclaim(ctx, cfg, cityPath, entry, "", safety)
		_ = head
		size, sizeErr := worktreeDirectorySize(entry.Path)
		if sizeErr != nil && reason == "" {
			reason = fmt.Sprintf("size probe failed: %v", sizeErr)
		}
		result = append(result, worktreeListEntry{
			worktreeRegistryEntry: entry,
			SizeBytes:             size,
			Reclaimable:           reason == "",
			Reason:                reason,
		})
	}
	return result, nil
}

func worktreeDirectorySize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}

func describeWorktreeEntryForOutput(ctx context.Context, cityPath string, cfg *config.City, entry worktreeRegistryEntry) worktreeListEntry {
	rows, err := listRegisteredWorktrees(ctx, cityPath, cfg, entry.ID, entry.Rig)
	if err == nil && len(rows) == 1 {
		return rows[0]
	}
	size, _ := worktreeDirectorySize(entry.Path)
	reason := "reclaim status unavailable"
	if err != nil {
		reason += ": " + err.Error()
	}
	return worktreeListEntry{
		worktreeRegistryEntry: entry,
		SizeBytes:             size,
		Reason:                reason,
	}
}

func writeWorktreeEntryOutput(stdout io.Writer, entry worktreeListEntry, jsonOutput bool) error {
	if jsonOutput {
		return writeWorktreeJSON(stdout, entry)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tOWNER\tRIG\tPATH\tHEAD_SHA\tCARGO_TARGET_DIR\tCARGO_HOME\tSIZE_BYTES\tPUBLISHED\tPUBLISHED_REF\tPUBLISHED_SHA\tRECLAIMABLE\tREASON"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%t\t%s\t%s\t%t\t%s\n", entry.ID, entry.Owner, entry.Rig, entry.Path, entry.HeadSHA, entry.CargoTargetDir, entry.CargoHome, entry.SizeBytes, entry.Published, entry.PublishedRef, entry.PublishedSHA, entry.Reclaimable, entry.Reason); err != nil {
		return err
	}
	return w.Flush()
}

func writeWorktreeReclaimOutput(stdout io.Writer, result worktreeReclaimResult, jsonOutput bool) error {
	if jsonOutput {
		return writeWorktreeJSON(stdout, result)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tOWNER\tRIG\tPATH\tRECLAIMED\tDRY_RUN\tRECLAIMABLE\tREASON"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%t\t%t\t%s\n", result.ID, result.Owner, result.Rig, result.Path, result.Reclaimed, result.DryRun, result.Reclaimable, result.Reason); err != nil {
		return err
	}
	return w.Flush()
}

func writeWorktreeListOutput(stdout io.Writer, entries []worktreeListEntry, jsonOutput bool) error {
	if jsonOutput {
		return writeWorktreeJSON(stdout, entries)
	}
	w := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tOWNER\tRIG\tPATH\tSIZE_BYTES\tPUBLISHED\tRECLAIMABLE\tREASON"); err != nil {
		return err
	}
	for i := range entries {
		entry := entries[i]
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%t\t%t\t%s\n", entry.ID, entry.Owner, entry.Rig, entry.Path, entry.SizeBytes, entry.Published, entry.Reclaimable, entry.Reason); err != nil {
			return err
		}
	}
	return w.Flush()
}

func writeWorktreeJSON(stdout io.Writer, value any) error {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func recordWorktreeCreated(rec events.Recorder, entry worktreeRegistryEntry) {
	if rec == nil {
		return
	}
	recordWorktreeEvent(rec, events.WorktreeCreated, entry.ID, events.WorktreeCreatedPayload{
		ID: entry.ID, Owner: entry.Owner, Rig: entry.Rig, Path: entry.Path, HeadSHA: entry.HeadSHA,
	})
}

func recordWorktreePublished(rec events.Recorder, entry worktreeRegistryEntry) {
	if rec == nil {
		return
	}
	recordWorktreeEvent(rec, events.WorktreePublished, entry.ID, events.WorktreePublishedPayload{
		ID: entry.ID, Owner: entry.Owner, Rig: entry.Rig, Path: entry.Path, Ref: entry.PublishedRef, HeadSHA: entry.PublishedSHA,
	})
}

func recordWorktreeReclaimSkipped(rec events.Recorder, entry worktreeRegistryEntry, head, reason string, dryRun bool) {
	if rec == nil {
		return
	}
	recordWorktreeEvent(rec, events.WorktreeReclaimSkipped, entry.ID, events.WorktreeReclaimSkippedPayload{
		ID: entry.ID, Owner: entry.Owner, Rig: entry.Rig, Path: entry.Path, HeadSHA: head, Reason: reason, DryRun: dryRun,
	})
}

func recordWorktreeReclaimed(rec events.Recorder, entry worktreeRegistryEntry, head string, dryRun bool) {
	if rec == nil {
		return
	}
	recordWorktreeEvent(rec, events.WorktreeReclaimed, entry.ID, events.WorktreeReclaimedPayload{
		ID: entry.ID, Owner: entry.Owner, Rig: entry.Rig, Path: entry.Path, HeadSHA: head, DryRun: dryRun,
	})
}

func recordWorktreeEvent(rec events.Recorder, eventType, subject string, payload any) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}
	rec.Record(events.Event{Type: eventType, Actor: eventActor(), Subject: subject, Payload: raw})
}
