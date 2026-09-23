package pipeline

// files.go writes a run's catalogues to disk and clears the previous run's.
//
// This is the frame's because three things have to agree on the filename
// pattern and only one of them is the collector's: what writes the files, what
// sweeps stale ones, and the glob the publish step sends from. A pipeline that
// chose its own pattern could silently publish nothing, or publish yesterday.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeCatalogs writes each built catalog to <dir>/<filenamePrefix>-<slug>.json,
// after clearing any catalog this run did not produce.
//
// Indented, because these files exist to be read: somebody reviews what is
// about to go onto the network, and a single-line document of several hundred
// markets cannot be reviewed. The publish step reads the same directory back,
// which is why the naming is fixed rather than a caller's choice.
//
// THE CLEARING IS NOT HOUSEKEEPING. The publish step globs every
// <filenamePrefix>-*.json in this directory and posts all of them, so a
// catalog left behind by an earlier run is republished as though it were
// current. Maharashtra splitting into MH and MH-2 on Monday and fitting into
// one catalog on Tuesday would, without this, republish Monday's MH-2 on
// Tuesday: a document of markets that no longer belong in one. A human
// running this by hand could see the directory and notice; the daily
// unattended loop this package exists for cannot, and the bug is invisible on
// the first run and wrong on every one after it.
func WriteCatalogues(built []Catalogue, dir, filenamePrefix string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create catalog output directory: %w", err)
	}

	if err := RemoveStaleCatalogues(dir, filenamePrefix); err != nil {
		return err
	}

	for _, catalog := range built {
		var indented bytes.Buffer
		if err := json.Indent(&indented, catalog.Content, "", "  "); err != nil {
			return fmt.Errorf("indent JSON for catalogue %s: %w", catalog.Slug, err)
		}

		path := filepath.Join(dir, fmt.Sprintf("%s-%s.json", filenamePrefix, catalog.Slug))
		if err := os.WriteFile(path, indented.Bytes(), 0o644); err != nil {
			return fmt.Errorf("write catalog file %s: %w", path, err)
		}
	}
	return nil
}

// removeStaleCatalogs deletes the catalogs already in dir, so only this run's
// output is left for the publish step to find.
//
// The match is deliberately the SAME one catalogpublish.catalogFiles makes --
// a non-directory entry whose name starts with "<filenamePrefix>-" and ends
// in ".json" -- because the set this removes has to be exactly the set that
// would otherwise be published. Matching more broadly would delete a file
// somebody else put here; matching more narrowly would leave one that still
// gets posted.
//
// Nothing else is touched: no recursion, no directories, and no file outside
// that pattern. This directory is an operator's to point wherever they like,
// and it may hold things that are not ours.
func RemoveStaleCatalogues(dir, filenamePrefix string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read catalog output directory: %w", err)
	}

	namePrefix := filenamePrefix + "-"
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, namePrefix) || !strings.HasSuffix(name, ".json") {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("remove stale catalog %s: %w", name, err)
		}
	}
	return nil
}
