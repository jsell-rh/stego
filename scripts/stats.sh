#!/usr/bin/env bash
# STEGO Build Stats — tracks velocity, quality, and cost metrics across tasks.
# Usage: ./scripts/stats.sh [--json]
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TASKS_DIR="$REPO_ROOT/specs/tasks"
REVIEWS_DIR="$REPO_ROOT/specs/reviews"
JSON_MODE=false
[[ "${1:-}" == "--json" ]] && JSON_MODE=true

# --- Colors (disabled for JSON or non-tty) ---
if $JSON_MODE; then
    BOLD="" DIM="" RESET="" GREEN="" YELLOW="" RED="" CYAN="" BLUE="" MAGENTA=""
else
    BOLD=$'\033[1m' DIM=$'\033[2m' RESET=$'\033[0m'
    GREEN=$'\033[32m' YELLOW=$'\033[33m' RED=$'\033[31m'
    CYAN=$'\033[36m' BLUE=$'\033[34m' MAGENTA=$'\033[35m'
fi

# --- Task status counts ---
total_tasks=0; complete=0; in_review=0; in_progress=0; not_started=0
declare -a task_names=() task_statuses=() task_titles=()

for f in "$TASKS_DIR"/task-*.md; do
    [[ -f "$f" ]] || continue
    total_tasks=$((total_tasks + 1))
    name=$(basename "$f" .md)
    status=$(grep -oP '(?<=\*\*Status:\*\* `)[^`]+' "$f" 2>/dev/null || echo "unknown")
    title=$(head -1 "$f" | sed 's/^# Task [0-9]*: //')
    # Replace em-dashes with plain dashes for consistent column width
    title="${title//—/-}"
    # Truncate to 28 chars with ellipsis in the middle
    if [[ ${#title} -gt 28 ]]; then
        title="${title:0:13}..${title: -13}"
    fi
    task_names+=("$name")
    task_statuses+=("$status")
    task_titles+=("$title")
    case "$status" in
        complete) complete=$((complete + 1)) ;;
        ready-for-review|in-review) in_review=$((in_review + 1)) ;;
        in-progress) in_progress=$((in_progress + 1)) ;;
        not-started) not_started=$((not_started + 1)) ;;
    esac
done

# --- Review metrics per task ---
declare -A review_rounds=() review_findings=() finding_categories=()

for f in "$REVIEWS_DIR"/task-*.md; do
    [[ -f "$f" ]] || continue
    name=$(basename "$f" .md)
    # Count review rounds: "## Round N" or "## Findings" (early format)
    # Exclude pass-only headers like "## Round 3 -- PASS" and "## Checklist"
    round_headers=$(grep -c '^## Round [0-9]' "$f" 2>/dev/null || true)
    findings_headers=$(grep -c '^## Findings' "$f" 2>/dev/null || true)
    rounds=$((${round_headers:-0} + ${findings_headers:-0}))
    # Count actual findings: lines with "[process-revision-complete]" tag
    # This distinguishes findings from checklist items which are also "- [x]"
    findings=$(grep -c 'process-revision-complete' "$f" 2>/dev/null || true)
    findings=${findings:-0}
    review_rounds[$name]=$((rounds))
    review_findings[$name]=$((findings))
done

# --- Wall clock time per task (from git history) ---
# Outlier threshold: gaps > 4 hours between consecutive commits within a task
# are excluded (likely sleep/connectivity breaks)
OUTLIER_GAP=3600  # 1 hour in seconds — gaps longer than this are likely context switches or sleep

declare -A task_wall_clock=() task_first_commit=() task_last_commit=()
declare -A task_active_seconds=()

declare -A task_commits=()

compute_task_time() {
    local task_id="$1"
    local commits
    commits=$(cd "$REPO_ROOT" && git log --all --format="%at" --grep="$task_id" --reverse 2>/dev/null)
    [[ -z "$commits" ]] && return

    local first last prev elapsed active_time=0 count=0
    while IFS= read -r ts; do
        [[ -z "$ts" ]] && continue
        if [[ $count -eq 0 ]]; then
            first=$ts
        else
            local gap=$((ts - prev))
            # Only count gaps under the outlier threshold
            if [[ $gap -gt 0 && $gap -lt $OUTLIER_GAP ]]; then
                active_time=$((active_time + gap))
            fi
        fi
        last=$ts
        prev=$ts
        count=$((count + 1))
    done <<< "$commits"

    [[ $count -lt 2 ]] && return

    task_first_commit[$task_id]=$first
    task_last_commit[$task_id]=$last
    task_wall_clock[$task_id]=$((last - first))
    task_active_seconds[$task_id]=$active_time
    task_commits[$task_id]=$count
}

for name in "${task_names[@]}"; do
    compute_task_time "$name"
done

# --- Code metrics ---
total_go_lines=$(find "$REPO_ROOT" -name "*.go" -not -path "*/.git/*" | xargs wc -l 2>/dev/null | tail -1 | awk '{print $1}')
test_go_lines=$(find "$REPO_ROOT" -name "*_test.go" -not -path "*/.git/*" | xargs wc -l 2>/dev/null | tail -1 | awk '{print $1}')
prod_go_lines=$((total_go_lines - test_go_lines))

total_commits=$(cd "$REPO_ROOT" && git log --all --oneline | wc -l)

# Overall timeline
first_commit_ts=$(cd "$REPO_ROOT" && git log --all --format="%at" --reverse | head -1)
last_commit_ts=$(cd "$REPO_ROOT" && git log --all --format="%at" | head -1)
total_wall_seconds=$((last_commit_ts - first_commit_ts))

# Checklist items (process learning)
checklist_items=$(cd "$REPO_ROOT" && git log --all --oneline --grep="fix(process)" | wc -l)

# --- Format helpers ---
fmt_duration() {
    local secs=$1
    local hrs=$((secs / 3600))
    local mins=$(( (secs % 3600) / 60 ))
    local s=$((secs % 60))
    if [[ $hrs -gt 0 ]]; then
        printf "%dh %dm %ds" "$hrs" "$mins" "$s"
    else
        printf "%dm %ds" "$mins" "$s"
    fi
}

progress_bar() {
    local total=$1 n_complete=$2 n_review=$3 n_progress=$4 n_not_started=$5
    local width=30
    # Calculate segment widths proportional to task counts
    local w_complete=$((n_complete * width / total))
    local w_review=$((n_review * width / total))
    local w_progress=$((n_progress * width / total))
    local w_not_started=$((width - w_complete - w_review - w_progress))

    local seg=""
    for ((i=0; i<w_complete; i++)); do seg+="#"; done
    printf "%s%s" "${GREEN}" "$seg"
    seg=""
    for ((i=0; i<w_review; i++)); do seg+="#"; done
    printf "%s%s" "${MAGENTA}" "$seg"
    seg=""
    for ((i=0; i<w_progress; i++)); do seg+="#"; done
    printf "%s%s" "${BLUE}" "$seg"
    seg=""
    for ((i=0; i<w_not_started; i++)); do seg+="-"; done
    printf "%s%s%s" "${DIM}" "$seg" "${RESET}"
}

# --- JSON output ---
if $JSON_MODE; then
    echo "{"
    echo "  \"summary\": {"
    echo "    \"total_tasks\": $total_tasks,"
    echo "    \"complete\": $complete,"
    echo "    \"in_review\": $in_review,"
    echo "    \"in_progress\": $in_progress,"
    echo "    \"not_started\": $not_started,"
    echo "    \"progress_pct\": $((complete * 100 / total_tasks)),"
    echo "    \"total_commits\": $total_commits,"
    echo "    \"prod_lines\": $prod_go_lines,"
    echo "    \"test_lines\": $test_go_lines,"
    echo "    \"test_ratio\": \"$(printf '%.1f' "$(echo "scale=1; $test_go_lines * 100 / $total_go_lines" | bc)")%\","
    echo "    \"total_wall_clock_seconds\": $total_wall_seconds,"
    echo "    \"checklist_items\": $checklist_items"
    echo "  },"
    echo "  \"tasks\": ["
    first=true
    for i in "${!task_names[@]}"; do
        name="${task_names[$i]}"
        status="${task_statuses[$i]}"
        rounds=${review_rounds[$name]:-0}
        findings=${review_findings[$name]:-0}
        active=${task_active_seconds[$name]:-0}
        wall=${task_wall_clock[$name]:-0}
        $first || echo ","
        first=false
        commits=${task_commits[$name]:-0}
        printf '    {"task": "%s", "status": "%s", "commits": %d, "review_rounds": %d, "findings": %d, "active_seconds": %d, "wall_seconds": %d}' \
            "$name" "$status" "$commits" "$rounds" "$findings" "$active" "$wall"
    done
    echo ""
    echo "  ]"
    echo "}"
    exit 0
fi

# --- Human output ---
echo ""
echo "${BOLD}STEGO Build Stats${RESET}"
echo "${DIM}$(date '+%Y-%m-%d %H:%M')${RESET}"
echo ""

# Progress
pct=$((complete * 100 / total_tasks))
echo "${BOLD}Progress${RESET}"
printf "  [$(progress_bar $total_tasks $complete $in_review $in_progress $not_started)] %d%% (%d/%d tasks)\n" "$pct" "$complete" "$total_tasks"
echo "  ${GREEN}$complete complete${RESET}  ${MAGENTA}$in_review in review${RESET}  ${BLUE}$in_progress in progress${RESET}  ${DIM}$not_started not started${RESET}"
echo ""

# Timeline
echo "${BOLD}Timeline${RESET}"
echo "  Total wall clock:  $(fmt_duration $total_wall_seconds)"
echo "  Total commits:     $total_commits"
echo "  Checklist items:   $checklist_items (accumulated process learnings)"
echo ""

# Code
echo "${BOLD}Code${RESET}"
echo "  Production:  $prod_go_lines lines"
echo "  Test:        $test_go_lines lines"
echo "  Test ratio:  $(printf '%.0f' "$(echo "scale=1; $test_go_lines * 100 / $total_go_lines" | bc)")% of total"
echo "  Throughput:  ~$(( prod_go_lines * 3600 / total_wall_seconds )) prod lines/hr"
echo ""

# Per-task breakdown
echo "${BOLD}Task Breakdown${RESET}"
printf "  ${DIM}%-12s  %-28s  %-16s  %7s  %6s  %8s  %14s${RESET}\n" "TASK" "TITLE" "STATUS" "COMMITS" "ROUNDS" "FINDINGS" "ACTIVE TIME"
printf "  %s%s%s\n" "${DIM}" "------------------------------------------------------------------------------------------------------" "${RESET}"

total_rounds=0; total_findings=0; total_active=0; total_task_commits=0
for i in "${!task_names[@]}"; do
    name="${task_names[$i]}"
    status="${task_statuses[$i]}"
    rounds=${review_rounds[$name]:-0}
    findings=${review_findings[$name]:-0}
    active=${task_active_seconds[$name]:-0}
    commits=${task_commits[$name]:-0}

    total_rounds=$((total_rounds + rounds))
    total_findings=$((total_findings + findings))
    total_active=$((total_active + active))
    total_task_commits=$((total_task_commits + commits))

    # Color code status — pad to exactly 16 visible chars
    case "$status" in
        complete)         status_label="complete";         status_color="$GREEN" ;;
        ready-for-review) status_label="ready-for-review"; status_color="$MAGENTA" ;;
        needs-revision)   status_label="needs-revision";   status_color="$YELLOW" ;;
        in-review)        status_label="in-review";        status_color="$YELLOW" ;;
        in-progress)      status_label="in-progress";      status_color="$BLUE" ;;
        not-started)      status_label="not-started";      status_color="$DIM" ;;
        *)                status_label="$status";        status_color="" ;;
    esac
    sc="${status_color}$(printf '%-16s' "$status_label")${RESET}"

    # Color code rounds by severity — right-align to 6 chars
    rpad=$(printf "%6d" "$rounds")
    if [[ $rounds -eq 0 ]]; then
        rc="${DIM}${rpad}${RESET}"
    elif [[ $rounds -le 3 ]]; then
        rc="${GREEN}${rpad}${RESET}"
    elif [[ $rounds -le 7 ]]; then
        rc="${YELLOW}${rpad}${RESET}"
    else
        rc="${RED}${rpad}${RESET}"
    fi

    fpad=$(printf "%8d" "$findings")

    if [[ $active -gt 0 ]]; then
        at=$(printf "%14s" "$(fmt_duration $active)")
    else
        at="$(printf '%12s' '')${DIM}--${RESET}"
    fi

    cpad=$(printf "%7d" "$commits")

    title="${task_titles[$i]}"
    printf "  %-12s  %-28s  %s  %s  %s  %s  %s\n" "$name" "$title" "$sc" "$cpad" "$rc" "$fpad" "$at"
done

printf "  %s%s%s\n" "${DIM}" "------------------------------------------------------------------------------------------------------" "${RESET}"
printf "  ${BOLD}%-12s  %-28s  %-16s  %7d  %6d  %8d  %14s${RESET}\n" "TOTAL" "" "" "$total_task_commits" "$total_rounds" "$total_findings" "$(fmt_duration $total_active)"
echo ""

# Review efficiency
if [[ $total_findings -gt 0 ]]; then
    echo "${BOLD}Review Efficiency${RESET}"
    echo "  Total findings:        $total_findings"
    echo "  Total review rounds:   $total_rounds"
    echo "  Findings per round:    $(printf '%.1f' "$(echo "scale=1; $total_findings / $total_rounds" | bc)")"
    echo "  Avg rounds per task:   $(printf '%.1f' "$(echo "scale=1; $total_rounds / $complete" | bc)") (completed tasks only)"
    echo ""

    # Top 3 most reviewed tasks
    echo "  ${DIM}Highest review effort:${RESET}"
    for name in "${task_names[@]}"; do
        r=${review_rounds[$name]:-0}
        f=${review_findings[$name]:-0}
        [[ $r -gt 0 ]] && echo "    $r rounds, $f findings  $name"
    done | sort -rn | head -3
    echo ""
fi

# Experiment comparison placeholder
echo "${BOLD}Experiment Comparison${RESET}"
echo "  ${DIM}Run this script after a second build to compare:${RESET}"
echo "  ${DIM}  ./scripts/stats.sh --json > run1.json${RESET}"
echo "  ${DIM}  # rebuild with updated prompts${RESET}"
echo "  ${DIM}  ./scripts/stats.sh --json > run2.json${RESET}"
echo "  ${DIM}  diff <(jq . run1.json) <(jq . run2.json)${RESET}"
echo ""
