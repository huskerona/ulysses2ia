// Copyright (c) 2026 Husein Roncevic
// SPDX-License-Identifier: MIT

// ulysses2ia converts a Ulysses iCloud library into a plain-Markdown tree
// suitable for iA Writer (or any folder-based Markdown editor).
//
// What it does
//   - Walks the Ulysses library root.
//   - For every group directory it finds an Info.ulgroup plist next to it and
//     uses the human-readable displayName as the destination folder name.
//   - For every .ulysses sheet bundle it reads Content.xml, reconstructs the
//     Markdown source from the XML character data, and writes a .md file
//     named after the sheet's first non-empty line (typically the H1).
//
// What it does NOT do
//   - Copy attachments (images, PDFs, etc.) inside sheet bundles.
//   - Preserve Ulysses keywords/notes as YAML front matter (easy to add — see
//     readSheetInfo).
//   - Touch anything in the source. It only reads.
//
// Usage
//
//	go run ulysses2ia.go \
//	  --src "$HOME/Library/Mobile Documents/XYAZV353FW~com~soulmen~ulysses3/Documents/Library" \
//	  --dst "$HOME/Documents/iA Writer" \
//	  --dry-run
//
// Drop --dry-run to actually write files. Always do a dry run first.
//
// Build a binary:
//
//	go build -o ulysses2ia ulysses2ia.go
//
// Requires macOS `plutil` (preinstalled). No third-party Go modules.
package main

import (
	"encoding/json"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ---------- plist helpers ----------

// readPlist converts a binary or XML plist to a generic map via `plutil`.
// macOS-only. Avoids pulling in a Go plist dependency.
func readPlist(path string) (map[string]any, error) {
	cmd := exec.Command("plutil", "-convert", "json", "-o", "-", path)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("plutil %s: %w", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		return nil, fmt.Errorf("decode plist %s: %w", path, err)
	}
	return m, nil
}

// readGroupName pulls the human-readable name out of an Info.ulgroup plist.
// Different Ulysses versions use slightly different keys, so try a few.
func readGroupName(infoPath string) (string, error) {
	m, err := readPlist(infoPath)
	if err != nil {
		return "", err
	}
	for _, k := range []string{"displayName", "name", "title"} {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v, nil
		}
	}
	return "", nil
}

// ---------- Content.xml -> Markdown ----------

// Content.xml example (Ulysses 3 XML format):
//
//   <?xml version="1.0" encoding="UTF-8"?>
//   <sheet version="6" xml:space="preserve">
//     <string>
//       <p>Some text with <element kind="inline" identifier="strong">**bold**</element></p>
//       <heading1><element kind="tag" identifier="heading1"># </element>Title</heading1>
//     </string>
//     ...
//   </sheet>
//
// Every Markdown syntax character lives as CharData inside an <element> or
// directly in the block tag. So if we walk the document and concatenate all
// CharData inside <string>, we get the original Markdown back.
//
// We just need to insert blank lines between block-level elements.

var blockTags = map[string]bool{
	"p":          true,
	"heading1":   true,
	"heading2":   true,
	"heading3":   true,
	"heading4":   true,
	"heading5":   true,
	"heading6":   true,
	"codeblock":  true,
	"blockquote": true,
	"comment":    true,
	"divider":    true,
	"rawsource":  true,
}

func extractMarkdown(contentPath string) (string, error) {
	f, err := os.Open(contentPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	dec := xml.NewDecoder(f)
	var sb strings.Builder
	inString := false

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("xml token: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "string" {
				inString = true
			}
		case xml.EndElement:
			if blockTags[t.Name.Local] {
				sb.WriteString("\n\n")
			}
			if t.Name.Local == "string" {
				inString = false
			}
		case xml.CharData:
			if inString {
				sb.Write(t)
			}
		}
	}

	s := sb.String()
	// Normalize: collapse 3+ blank lines, trim trailing whitespace per line.
	s = regexp.MustCompile(`\n{3,}`).ReplaceAllString(s, "\n\n")
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = strings.TrimRight(ln, " \t")
	}
	s = strings.Join(lines, "\n")
	return strings.TrimSpace(s) + "\n", nil
}

// ---------- filenames ----------

var (
	illegalChars = regexp.MustCompile(`[/\\:*?"<>|]+`)
	multiSpace   = regexp.MustCompile(`\s+`)
)

func sanitizeFilename(s string) string {
	s = strings.TrimSpace(s)
	// Strip leading Markdown heading markers so "# Title" becomes "Title".
	for strings.HasPrefix(s, "#") {
		s = strings.TrimPrefix(s, "#")
		s = strings.TrimSpace(s)
	}
	s = illegalChars.ReplaceAllString(s, "-")
	s = multiSpace.ReplaceAllString(s, " ")
	s = strings.Trim(s, " .-")
	if len(s) > 120 {
		s = strings.TrimSpace(s[:120])
	}
	return s
}

func firstNonEmptyLine(md string) string {
	for _, ln := range strings.Split(md, "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			return ln
		}
	}
	return ""
}

// ---------- walking ----------

type walker struct {
	dryRun bool
	stats  struct {
		groups, sheets, skipped int
	}
}

func (w *walker) walkGroup(src, dst string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })

	// Collision tracker for sheets written into this directory.
	used := map[string]int{}

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		full := filepath.Join(src, e.Name())

		switch {
		case strings.HasSuffix(e.Name(), ".ulysses"):
			if err := w.convertSheet(full, dst, used); err != nil {
				fmt.Fprintf(os.Stderr, "warn: %s: %v\n", full, err)
				w.stats.skipped++
			}

		default:
			// Group folder? Look for Info.ulgroup.
			infoPath := filepath.Join(full, "Info.ulgroup")
			if _, err := os.Stat(infoPath); err == nil {
				name, _ := readGroupName(infoPath)
				if name == "" {
					name = e.Name() // fall back to UUID
				}
				safe := sanitizeFilename(name)
				if safe == "" {
					safe = e.Name()
				}
				subDst := filepath.Join(dst, safe)
				fmt.Printf("group: %-50s -> %s\n", name, subDst)
				if !w.dryRun {
					if err := os.MkdirAll(subDst, 0o755); err != nil {
						return err
					}
				}
				w.stats.groups++
				if err := w.walkGroup(full, subDst); err != nil {
					return err
				}
			} else {
				// Unknown directory — recurse anyway, sheets might live deeper.
				if err := w.walkGroup(full, dst); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (w *walker) convertSheet(sheetDir, dst string, used map[string]int) error {
	contentPath := filepath.Join(sheetDir, "Content.xml")
	if _, err := os.Stat(contentPath); err != nil {
		return fmt.Errorf("no Content.xml: %w", err)
	}

	md, err := extractMarkdown(contentPath)
	if err != nil {
		return err
	}

	title := sanitizeFilename(firstNonEmptyLine(md))
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(sheetDir), ".ulysses")
	}

	used[title]++
	fname := title + ".md"
	if used[title] > 1 {
		fname = fmt.Sprintf("%s (%d).md", title, used[title])
	}

	outPath := filepath.Join(dst, fname)
	fmt.Printf("  sheet: %s\n", outPath)
	w.stats.sheets++
	if w.dryRun {
		return nil
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return os.WriteFile(outPath, []byte(md), 0o644)
}

// ---------- main ----------

func main() {
	src := flag.String("src", "", "Ulysses library root, e.g. ~/Library/Mobile Documents/XYAZV353FW~com~soulmen~ulysses3/Documents/Library")
	dst := flag.String("dst", "", "destination directory for the Markdown tree")
	dryRun := flag.Bool("dry-run", false, "print actions without writing files")
	flag.Parse()

	if *src == "" || *dst == "" {
		fmt.Fprintln(os.Stderr, "usage: ulysses2ia --src <path> --dst <path> [--dry-run]")
		flag.PrintDefaults()
		os.Exit(2)
	}

	srcAbs, err := expandPath(*src)
	if err != nil {
		fatal(err)
	}
	dstAbs, err := expandPath(*dst)
	if err != nil {
		fatal(err)
	}

	if !*dryRun {
		if err := os.MkdirAll(dstAbs, 0o755); err != nil {
			fatal(err)
		}
	}

	w := &walker{dryRun: *dryRun}
	if err := w.walkGroup(srcAbs, dstAbs); err != nil {
		fatal(err)
	}

	fmt.Fprintf(os.Stderr, "\ndone. groups: %d, sheets: %d, skipped: %d, dry-run: %v\n",
		w.stats.groups, w.stats.sheets, w.stats.skipped, *dryRun)
}

func expandPath(p string) (string, error) {
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return filepath.Abs(p)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(1)
}
