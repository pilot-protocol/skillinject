// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
)

// SkillFormatMuse is the ManifestGatedTool.SkillFormat value for Meta
// Muse. Muse is known to load a SKILL.md whose frontmatter is exactly
//
//	---
//	name: "<entrypoint with - replaced by _>"
//	description: "<one line>"
//	---
//
// (a quoted name that differs from the folder name, a one-line quoted
// description, no folded YAML). museSkillMD produces that shape.
const SkillFormatMuse = "muse"

// museDescriptionMax is the byte cap on the rewritten description.
const museDescriptionMax = 1024

// formatSkill applies a gated tool's SkillFormat to the entrypoint
// SKILL.md. entrypoint is the manifest entrypoint (the skill's folder
// name).
func formatSkill(body []byte, format, entrypoint string) ([]byte, error) {
	switch format {
	case "":
		return body, nil
	case SkillFormatMuse:
		return museSkillMD(body, strings.ReplaceAll(entrypoint, "-", "_")), nil
	default:
		return nil, fmt.Errorf("unsupported skillFormat %q", format)
	}
}

// museSkillMD rewrites the frontmatter of a SKILL.md into the shape Muse
// loads (see SkillFormatMuse) and keeps the body byte for byte. Every
// frontmatter key other than name and description is dropped. A file
// that does not start with a closed "---" block is returned unchanged.
//
// It is a port of muse_frontmatter in pilot-skills muse/install.sh, which
// rewrites the skills that installer copies into ~/workspace/skills. The
// two have to produce the same bytes: otherwise the daemon rewrites the
// installer's copy on its first tick and the installer puts its own back
// on every rerun. The steps below follow that function in order, and
// TestMuseSkillMD_MatchesInstaller runs both on the same inputs.
func museSkillMD(src []byte, name string) []byte {
	lines, offsets := splitRecords(src)
	if len(lines) == 0 || !isFenceLine(lines[0]) {
		return src
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if isFenceLine(lines[i]) {
			end = i
			break
		}
	}
	if end < 0 {
		return src
	}

	desc := normalizeMuseDescription(extractDescription(lines[1:end]), name)

	// The body is everything after the closing fence's line, as
	// `tail -n +<end+1>` prints it.
	body := []byte(nil)
	if end+1 < len(offsets) {
		body = src[offsets[end+1]:]
	}

	var out bytes.Buffer
	out.Grow(len(name) + len(desc) + len(body) + 40)
	out.WriteString("---\nname: \"")
	out.WriteString(name)
	out.WriteString("\"\ndescription: \"")
	out.WriteString(desc)
	out.WriteString("\"\n---\n")
	out.Write(body)
	return out.Bytes()
}

// splitRecords splits s into awk records (lines without their "\n"; a
// trailing "\n" does not start another record) and returns the byte
// offset at which each record starts.
func splitRecords(s []byte) ([]string, []int) {
	var lines []string
	var offsets []int
	start := 0
	for start < len(s) {
		offsets = append(offsets, start)
		i := bytes.IndexByte(s[start:], '\n')
		if i < 0 {
			lines = append(lines, string(s[start:]))
			start = len(s)
			break
		}
		lines = append(lines, string(s[start:start+i]))
		start += i + 1
	}
	return lines, offsets
}

// isFenceLine matches awk's /^---\r?$/.
func isFenceLine(l string) bool {
	return l == "---" || l == "---\r"
}

var (
	// descBlockRE is a block scalar indicator (| or >, with optional
	// chomping/indent indicators and a trailing comment).
	descBlockRE = regexp.MustCompile(`^[|>][-+0-9]*([ \t]+#.*)?$`)
	// descCommentRE is a trailing " # comment" on a plain scalar.
	descCommentRE = regexp.MustCompile(`[ \t]#.*$`)
)

// trimBlank trims spaces and tabs, as the installer's awk trim().
func trimBlank(s string) string {
	return strings.Trim(s, " \t")
}

// extractDescription returns the raw description value from the
// frontmatter lines between the fences: the text after "description:",
// with continuation lines (indented or empty) trimmed and joined by one
// space. Block scalars and quoted scalars keep " #"; plain scalars lose
// a trailing comment.
func extractDescription(lines []string) string {
	const (
		seeking = iota
		collecting
		done
	)
	state := seeking
	block, quoted := false, false
	value := ""
	for _, l := range lines {
		l = strings.TrimSuffix(l, "\r")
		if state == collecting {
			if strings.HasPrefix(l, " ") || strings.HasPrefix(l, "\t") || l == "" {
				line := trimBlank(l)
				if !block && !quoted {
					line = stripComment(line)
				}
				if line != "" {
					value += " " + line
				}
				continue
			}
			state = done
		}
		if state == seeking && strings.HasPrefix(l, "description:") {
			v := trimBlank(strings.TrimPrefix(l, "description:"))
			switch {
			case descBlockRE.MatchString(v):
				block = true
				v = ""
			case strings.HasPrefix(v, `"`) || strings.HasPrefix(v, "'"):
				quoted = true
			default:
				v = stripComment(v)
			}
			value = v
			state = collecting
		}
	}
	return value
}

func stripComment(s string) string {
	if loc := descCommentRE.FindStringIndex(s); loc != nil {
		return s[:loc[0]]
	}
	return s
}

// normalizeMuseDescription turns the raw value into the one-line,
// double-quote-escaped text written after description:.
func normalizeMuseDescription(v, name string) string {
	// tr -d '\000-\010\013-\037' | tr '\t\n' '  ' | tr -s ' '
	b := make([]byte, 0, len(v))
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c <= 0x08, c >= 0x0b && c <= 0x1f:
			continue
		case c == '\t', c == '\n':
			c = ' '
		}
		if c == ' ' && len(b) > 0 && b[len(b)-1] == ' ' {
			continue
		}
		b = append(b, c)
	}
	d := string(b)
	d = strings.TrimPrefix(d, " ")
	d = strings.TrimSuffix(d, " ")

	// Unquote a quoted scalar.
	switch {
	case len(d) >= 2 && d[0] == '"' && d[len(d)-1] == '"':
		d = d[1 : len(d)-1]
		d = strings.ReplaceAll(d, `\\`, "\x01")
		d = strings.ReplaceAll(d, `\"`, `"`)
		d = strings.ReplaceAll(d, `\n`, " ")
		d = strings.ReplaceAll(d, `\t`, " ")
		d = strings.ReplaceAll(d, "\x01", `\`)
	case len(d) >= 2 && d[0] == '\'' && d[len(d)-1] == '\'':
		d = d[1 : len(d)-1]
		d = strings.ReplaceAll(d, "''", "'")
	}

	if d == "" {
		d = "Pilot Protocol skill " + name
	}
	if len(d) > museDescriptionMax {
		d = d[:museDescriptionMax-4]
		if i := strings.LastIndex(d, " "); i >= 0 {
			d = d[:i]
		}
		d += "..."
	}
	d = strings.ReplaceAll(d, `\`, `\\`)
	d = strings.ReplaceAll(d, `"`, `\"`)
	return d
}
