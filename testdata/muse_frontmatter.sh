# muse_frontmatter, copied verbatim from pilot-skills muse/install.sh
# (TeoSlayer/pilot-skills@71789adfe784d0f55ae592f137e4b1aabc45be97, PR #34).
# The Muse installer rewrites the skills it copies into ~/workspace/skills with
# this function; museSkillMD (skillformat.go) must produce the same bytes.
# TestMuseSkillMD_MatchesInstaller sources this file and compares the two.
# If the installer's function changes, copy it here again.

muse_frontmatter() {
  local file="$1" name="${2:-}" end desc
  if [ -z "$name" ]; then
    name="$(basename "$(dirname "$file")")"
    name="${name//-/_}"
  fi
  end="$(awk 'NR == 1 { if ($0 !~ /^---\r?$/) exit; next } /^---\r?$/ { print NR; exit }' "$file")" || return 1
  [ -n "$end" ] || return 0
  desc="$(awk -v end="$end" '
    function trim(s) { sub(/^[ \t]+/, "", s); sub(/[ \t]+$/, "", s); return s }
    NR == 1 { next }
    NR >= end { exit }
    {
      sub(/\r$/, "")
      if (state == 1) {
        if ($0 ~ /^[ \t]/ || $0 == "") {
          line = trim($0)
          if (!block && !quoted) sub(/[ \t]#.*$/, "", line)
          if (line != "") value = value " " line
          next
        }
        state = 2
      }
      if (state == 0 && $0 ~ /^description:/) {
        v = $0
        sub(/^description:/, "", v)
        v = trim(v)
        if (v ~ /^[|>][-+0-9]*([ \t]+#.*)?$/) { block = 1; v = "" }
        else if (v ~ /^["\047]/) quoted = 1
        else sub(/[ \t]#.*$/, "", v)
        value = v
        state = 1
      }
    }
    END { printf "%s", value }
  ' "$file")" || return 1
  desc="$(printf '%s' "$desc" | tr -d '\000-\010\013-\037' | tr '\t\n' '  ' | tr -s ' ')"
  desc="${desc# }"
  desc="${desc% }"
  case "$desc" in
    \"*\")
      desc="${desc#\"}"
      desc="${desc%\"}"
      desc="${desc//\\\\/$'\001'}" # protect escaped backslashes
      desc="${desc//\\\"/\"}"
      desc="${desc//\\n/ }"
      desc="${desc//\\t/ }"
      desc="${desc//$'\001'/\\}"
      ;;
    \'*\')
      local q="'"
      desc="${desc#"$q"}"
      desc="${desc%"$q"}"
      desc="${desc//"$q$q"/$q}"
      ;;
  esac
  if [ -z "$desc" ]; then desc="Pilot Protocol skill ${name}"; fi
  if [ "$(printf '%s' "$desc" | LC_ALL=C wc -c | tr -d ' ')" -gt 1024 ]; then
    desc="$(printf '%s' "$desc" | LC_ALL=C cut -c1-1020)"
    desc="${desc% *}..."
  fi
  desc="${desc//\\/\\\\}"
  desc="${desc//\"/\\\"}"
  {
    printf -- '---\nname: "%s"\ndescription: "%s"\n---\n' "$name" "$desc"
    tail -n "+$((end + 1))" "$file"
  } > "$file.muse-tmp" && mv -f "$file.muse-tmp" "$file"
}
