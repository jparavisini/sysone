package main

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

//go:embed skills/*/SKILL.md specs/*.json
var assets embed.FS

const skillsUsage = `sysone skills - agent skills bundled in the binary

Usage:
  sysone skills list
  sysone skills print NAME
  sysone skills install   [--agent claude|codex|generic] [--dir PATH] [--force]
  sysone skills uninstall [--agent claude] [--dir PATH]

Targets:
  claude   (default) writes ~/.claude/skills/NAME/SKILL.md for each skill
  codex    appends an AGENTS.md snippet to --dir (default: current directory)
  generic  prints the AGENTS.md snippet to stdout

Examples:
  sysone skills install
  sysone skills install --agent codex --dir ~/myproject
  sysone skills print sysone-specs | less
`

func skillNames() []string {
	entries, _ := fs.ReadDir(assets, "skills")
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

func skillBody(name string) ([]byte, error) {
	b, err := assets.ReadFile("skills/" + name + "/SKILL.md")
	if err != nil {
		return nil, fail(exitUsage, "unknown skill %q; see `sysone skills list`", name)
	}
	return b, nil
}

func exampleSpecNames() []string {
	entries, _ := fs.ReadDir(assets, "specs")
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names
}

func exampleSpec(name string) []byte {
	b, _ := assets.ReadFile("specs/" + name + ".json")
	return b
}

func cmdSkills(args []string) error {
	var agent, dir string
	var force bool
	fs := newFlagSet("skills", skillsUsage)
	fs.StringVar(&agent, "agent", "claude", "target agent")
	fs.StringVar(&dir, "dir", "", "target directory")
	fs.BoolVar(&force, "force", false, "overwrite existing files")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 0 {
		fmt.Fprint(stdout, skillsUsage)
		return &exitError{code: exitUsage}
	}
	switch pos[0] {
	case "list":
		for _, n := range skillNames() {
			b, _ := skillBody(n)
			fmt.Fprintf(stdout, "%-16s %s\n", n, frontMatter(b, "description"))
		}
		return nil
	case "print":
		if len(pos) != 2 {
			return fail(exitUsage, "usage: sysone skills print NAME")
		}
		b, err := skillBody(pos[1])
		if err != nil {
			return err
		}
		_, err = stdout.Write(b)
		return err
	case "install":
		return skillsInstall(agent, dir, force)
	case "uninstall":
		return skillsUninstall(agent, dir)
	}
	return fail(exitUsage, "unknown skills subcommand %q\n\n%s", pos[0], skillsUsage)
}

func claudeSkillsDir(dir string) string {
	if dir != "" {
		return dir
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "skills")
}

func skillsInstall(agent, dir string, force bool) error {
	switch agent {
	case "claude":
		root := claudeSkillsDir(dir)
		for _, n := range skillNames() {
			b, _ := skillBody(n)
			dst := filepath.Join(root, n, "SKILL.md")
			if _, err := os.Stat(dst); err == nil && !force {
				fmt.Fprintf(stderr, "kept %s (use --force to overwrite)\n", dst)
				continue
			}
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return fail(exitBadInput, "%v", err)
			}
			if err := os.WriteFile(dst, b, 0o644); err != nil {
				return fail(exitBadInput, "%v", err)
			}
			fmt.Fprintf(stderr, "wrote %s\n", dst)
		}
		return nil
	case "codex":
		if dir == "" {
			dir = "."
		}
		dst := filepath.Join(dir, "AGENTS.md")
		existing, err := os.ReadFile(dst)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fail(exitBadInput, "%v", err)
		}
		if strings.Contains(string(existing), agentsMarker) && !force {
			fmt.Fprintf(stderr, "kept %s: sysone section already present (use --force to append again)\n", dst)
			return nil
		}
		f, err := os.OpenFile(dst, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fail(exitBadInput, "%v", err)
		}
		defer f.Close()
		if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
			fmt.Fprintln(f)
		}
		if _, err := f.WriteString(agentsSnippet()); err != nil {
			return fail(exitBadInput, "%v", err)
		}
		fmt.Fprintf(stderr, "appended sysone section to %s\n", dst)
		return nil
	case "generic":
		_, err := fmt.Fprint(stdout, agentsSnippet())
		return err
	}
	return fail(exitUsage, "unknown --agent %q (claude|codex|generic)", agent)
}

func skillsUninstall(agent, dir string) error {
	if agent != "claude" {
		return fail(exitUsage, "uninstall only supports --agent claude; edit AGENTS.md by hand for codex")
	}
	root := claudeSkillsDir(dir)
	for _, n := range skillNames() {
		p := filepath.Join(root, n)
		if _, err := os.Stat(filepath.Join(p, "SKILL.md")); err != nil {
			continue
		}
		if err := os.RemoveAll(p); err != nil {
			return fail(exitBadInput, "%v", err)
		}
		fmt.Fprintf(stderr, "removed %s\n", p)
	}
	return nil
}

const agentsMarker = "<!-- sysone skills -->"

// agentsSnippet concatenates every skill body (front matter stripped) under
// one marked section for AGENTS.md.
func agentsSnippet() string {
	var b strings.Builder
	b.WriteString("\n" + agentsMarker + "\n")
	for _, n := range skillNames() {
		body, _ := skillBody(n)
		b.WriteString(stripFrontMatter(string(body)))
		b.WriteString("\n")
	}
	b.WriteString("<!-- /sysone skills -->\n")
	return b.String()
}

// frontMatter reads one scalar key from a SKILL.md YAML header.
func frontMatter(b []byte, key string) string {
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, key+":") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, key+":")), `"`)
		}
	}
	return ""
}

func stripFrontMatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	rest := s[4:]
	i := strings.Index(rest, "\n---\n")
	if i < 0 {
		return s
	}
	return strings.TrimLeft(rest[i+5:], "\n")
}
