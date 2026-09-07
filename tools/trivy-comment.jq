# Render a Trivy SARIF report as a markdown table for a PR comment.
#
# Used by the Makefile's trivy-comment target for both the dependency and the
# image scan — one program, not a copy per scan.
#
# Trivy's SARIF carries no structured per-field severity/version columns: each
# finding's detail lives as prose in message.text ("Package: ...\nSeverity:
# ...\n..."), so the columns below are pulled out of that text rather than read
# from dedicated JSON fields.
#
# Lint with:  jq -n -f tools/trivy-comment.jq

# capture returns null when the pattern doesn't match, so `// {v: default}`
# supplies the fallback rather than letting a missing field print "null".
def val(re; default): (capture(re) // {v: default}).v;

[ .runs[]?.results[]? ] as $found
| if ($found | length) == 0 then
    "No HIGH or CRITICAL findings."
  else
    "| Package | Severity | Installed | Fixed in | Advisory |",
    "|---|---|---|---|---|",
    ( $found[]
      | .ruleId as $id
      | (.message.text // "") as $m
      | "| `" + ($m | val("Package: (?<v>[^\\n]+)"; "?")) + "` "
      + "| "  + ($m | val("Severity: (?<v>[^\\n]+)"; "?")) + " "
      + "| "  + ($m | val("Installed Version: (?<v>[^\\n]+)"; "?")) + " "
      + "| "  + ($m | val("Fixed Version: (?<v>[^\\n]+)"; "—")) + " "
      + "| [" + $id + "](" + ($m | val("Link: \\[[^]]+\\]\\((?<v>[^)]+)\\)"; "")) + ") |"
    )
  end
