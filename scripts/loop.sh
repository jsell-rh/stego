while true; do 
  claude --model opus[1m] --dangerously-skip-permissions < specs/prompts/project-manager.md
  claude --model opus[1m] --dangerously-skip-permissions < specs/prompts/implementation.md
  claude --model opus[1m] --dangerously-skip-permissions < specs/prompts/verifier.md
  claude --model opus[1m] --dangerously-skip-permissions < specs/prompts/process-revision.md
done
