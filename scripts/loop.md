while true; do 
  claude --model opus[1m] --dangerously-skip-permissions "$(cat specs/prompts/project-manager.md)";
  claude --model opus[1m] --dangerously-skip-permissions "$(cat specs/prompts/implementation.md)";
  claude --model opus[1m] --dangerously-skip-permissions "$(cat specs/prompts/verifier.md)";
  claude --model opus[1m] --dangerously-skip-permissions "$(cat specs/prompts/process-revision.md)";
done
