// SPDX-License-Identifier: AGPL-3.0-or-later

package skillinject

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// museCases are SKILL.md inputs covering every branch of the Muse
// frontmatter rewrite. TestMuseSkillMD_MatchesInstaller runs each through
// the installer's shell function as well.
var museCases = map[string]string{
	"folded":             "---\nname: pilotctl\ndescription: >\n  Entrypoint for Pilot Protocol, the overlay\n  network. Load it for `pilotctl`.\ntags:\n  - pilot-protocol\nlicense: AGPL-3.0\nallowed-tools:\n  - Bash\n---\n\n# pilotctl\n\nbody line\n",
	"plain-comment":      "---\nname: x\ndescription: Plain text # a comment\n---\nbody\n",
	"double-quoted":      "---\nname: x\ndescription: \"Say \\\"hi\\\" \\\\ then\\nnew\\tline # not a comment\"\n---\nbody\n",
	"single-quoted":      "---\nname: x\ndescription: 'It''s quoted # kept'\n---\nbody\n",
	"plain-multiline":    "---\nname: x\ndescription: first part\n  second part # dropped\n\n  third\nlicense: MIT\n---\nbody\n",
	"literal-block":      "---\nname: x\ndescription: |- # comment\n  line one\n\n  line two # kept\nlicense: MIT\n---\nbody\n",
	"empty-value-indent": "---\nname: x\ndescription:\n  continued value\n---\nbody\n",
	"no-description":     "---\nname: x\nlicense: MIT\n---\nbody\n",
	"crlf":               "---\r\nname: x\r\ndescription: >\r\n  crlf folded\r\n  text\r\n---\r\nbody\r\nmore\r\n",
	"no-frontmatter":     "# Title\n\ndescription: not frontmatter\n",
	"unclosed":           "---\nname: x\ndescription: never closed\n",
	"fence-trailing-ws":  "--- \nname: x\ndescription: y\n---\nbody\n",
	"empty":              "",
	"only-fences":        "---\n---\n",
	"fence-at-eof":       "---\nname: x\ndescription: at eof\n---",
	"quotes-in-plain":    "---\nname: x\ndescription: say \"yes\" to C:\\path\n---\nbody\n",
	"quoted-then-cmt":    "---\nname: x\ndescription: \"quoted\" # trailing\n---\nbody\n",
	"control-chars":      "---\nname: x\ndescription: a\x01b\x0bc\td  e\n---\nbody\n",
	"key-after":          "---\nname: x\ntags:\n  - a\ndescription: middle\nlicense: MIT\ndescription: second is ignored\n---\nbody\n",
	"long-spaces":        "---\nname: x\ndescription: " + strings.Repeat("word ", 300) + "\n---\nbody\n",
	"long-no-space":      "---\nname: x\ndescription: " + strings.Repeat("x", 1500) + "\n---\nbody\n",
	"long-exact":         "---\nname: x\ndescription: " + strings.Repeat("y", 1024) + "\n---\nbody\n",
	"long-utf8":          "---\nname: x\ndescription: " + strings.Repeat("é ab ", 250) + "\n---\nbody\n",
	"body-keeps-fences":  "---\nname: x\ndescription: d\n---\n---\nnot: frontmatter\n---\n",
}

func TestMuseSkillMD_Golden(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, in, want string }{
		{"folded", museCases["folded"],
			"---\nname: \"pilotctl\"\ndescription: \"Entrypoint for Pilot Protocol, the overlay network. Load it for `pilotctl`.\"\n---\n\n# pilotctl\n\nbody line\n"},
		{"plain-comment", museCases["plain-comment"],
			"---\nname: \"pilotctl\"\ndescription: \"Plain text\"\n---\nbody\n"},
		{"double-quoted", museCases["double-quoted"],
			"---\nname: \"pilotctl\"\ndescription: \"Say \\\"hi\\\" \\\\ then new line # not a comment\"\n---\nbody\n"},
		{"single-quoted", museCases["single-quoted"],
			"---\nname: \"pilotctl\"\ndescription: \"It's quoted # kept\"\n---\nbody\n"},
		{"plain-multiline", museCases["plain-multiline"],
			"---\nname: \"pilotctl\"\ndescription: \"first part second part third\"\n---\nbody\n"},
		{"no-description", museCases["no-description"],
			"---\nname: \"pilotctl\"\ndescription: \"Pilot Protocol skill pilotctl\"\n---\nbody\n"},
		{"crlf", museCases["crlf"],
			"---\nname: \"pilotctl\"\ndescription: \"crlf folded text\"\n---\nbody\r\nmore\r\n"},
		{"no-frontmatter", museCases["no-frontmatter"], museCases["no-frontmatter"]},
		{"unclosed", museCases["unclosed"], museCases["unclosed"]},
		{"empty", "", ""},
		{"fence-at-eof", museCases["fence-at-eof"],
			"---\nname: \"pilotctl\"\ndescription: \"at eof\"\n---\n"},
		{"quotes-in-plain", museCases["quotes-in-plain"],
			"---\nname: \"pilotctl\"\ndescription: \"say \\\"yes\\\" to C:\\\\path\"\n---\nbody\n"},
		{"control-chars", museCases["control-chars"],
			"---\nname: \"pilotctl\"\ndescription: \"abc d e\"\n---\nbody\n"},
		{"body-keeps-fences", museCases["body-keeps-fences"],
			"---\nname: \"pilotctl\"\ndescription: \"d\"\n---\n---\nnot: frontmatter\n---\n"},
	}
	for _, c := range cases {
		if got := string(museSkillMD([]byte(c.in), "pilotctl")); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestMuseSkillMD_LongDescriptionIsCapped(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"long-spaces", "long-no-space", "long-utf8"} {
		out := string(museSkillMD([]byte(museCases[name]), "x"))
		line := strings.Split(out, "\n")[2]
		desc := strings.TrimSuffix(strings.TrimPrefix(line, `description: "`), `"`)
		if len(desc) > museDescriptionMax {
			t.Errorf("%s: description is %d bytes, want <= %d", name, len(desc), museDescriptionMax)
		}
		if !strings.HasSuffix(desc, "...") {
			t.Errorf("%s: capped description should end in ...: %q", name, desc[len(desc)-10:])
		}
	}
	// Exactly at the cap is kept whole.
	out := string(museSkillMD([]byte(museCases["long-exact"]), "x"))
	if !strings.Contains(out, strings.Repeat("y", 1024)+"\"") {
		t.Error("a 1024-byte description was cut")
	}
}

func TestFormatSkill(t *testing.T) {
	t.Parallel()
	in := []byte(museCases["folded"])
	if got, err := formatSkill(in, "", "pilot-ctl"); err != nil || !bytes.Equal(got, in) {
		t.Errorf("empty format should copy verbatim, got %q, %v", got, err)
	}
	got, err := formatSkill(in, SkillFormatMuse, "pilot-ctl")
	if err != nil || !strings.HasPrefix(string(got), "---\nname: \"pilot_ctl\"\n") {
		t.Errorf("muse format should name the skill pilot_ctl, got %q, %v", got, err)
	}
	if _, err := formatSkill(in, "other", "pilotctl"); err == nil {
		t.Error("unknown format should be an error")
	}
}

// TestMuseSkillMD_MatchesInstaller runs the Muse installer's own
// muse_frontmatter (testdata/muse_frontmatter.sh) and museSkillMD on the
// same inputs and requires identical bytes. Set PILOT_SKILLS_CORPUS to a
// pilot-skills checkout to also compare every skills/*/SKILL.md in it.
func TestMuseSkillMD_MatchesInstaller(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	for _, tool := range []string{"awk", "tr", "cut", "wc", "tail", "mv"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not found", tool)
		}
	}
	fn, err := filepath.Abs(filepath.Join("testdata", "muse_frontmatter.sh"))
	if err != nil {
		t.Fatal(err)
	}

	inputs := map[string][]byte{}
	for name, body := range museCases {
		inputs[name] = []byte(body)
	}
	if dir := os.Getenv("PILOT_SKILLS_CORPUS"); dir != "" {
		files, _ := filepath.Glob(filepath.Join(dir, "skills", "*", "SKILL.md"))
		if len(files) == 0 {
			t.Fatalf("PILOT_SKILLS_CORPUS=%s has no skills/*/SKILL.md", dir)
		}
		for _, f := range files {
			b, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			inputs["corpus:"+filepath.Base(filepath.Dir(f))] = b
		}
		t.Logf("comparing %d corpus files from %s", len(files), dir)
	}

	tmp := t.TempDir()
	i := 0
	for name, body := range inputs {
		i++
		skill := "pilot-ctl"
		p := filepath.Join(tmp, fmt.Sprintf("c%04d", i), skill, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, body, 0o644); err != nil {
			t.Fatal(err)
		}
		// Same shell options and call as the installer:
		// muse_frontmatter "$dest/$skill/SKILL.md" "${skill//-/_}".
		cmd := exec.Command(bash, "-c", `set -euo pipefail; . "$0"; muse_frontmatter "$1" "$2"`, fn, p, "pilot_ctl")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s: installer function failed: %v\n%s", name, err, out)
			continue
		}
		want, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := museSkillMD(body, "pilot_ctl"); !bytes.Equal(got, want) {
			t.Errorf("%s: museSkillMD differs from the installer\n got %q\nwant %q", name, head(got), head(want))
		}
	}
}

// head returns the frontmatter part of a SKILL.md for failure messages.
func head(b []byte) string {
	if len(b) > 1400 {
		return string(b[:1400]) + "..."
	}
	return string(b)
}
