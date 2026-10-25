#!/bin/sh
# TEST FIXTURE — scripted fake agent CLI standing in for `pi`/`omp` in hermetic tests.
# Mimics `--version` and `--mode json -p ... <prompt>` NDJSON output. Not a real agent.
# Behaviour is selected by markers in the final argument (the prompt).
if [ "$1" = "--version" ]; then echo "fake-agent 9.9.9"; exit 0; fi

for arg in "$@"; do prompt="$arg"; done

echo '{"type":"agent_start"}'
echo 'raw non-json noise line'

case "$prompt" in
  *MODE_FAIL*)
    echo "fake agent exploded" >&2
    exit 3
    ;;
  *MODE_HANG*)
    echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hanging now"}]}}'
    exec sleep 300
    ;;
  *MODE_IGNORE_TERM*)
    trap '' TERM
    echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ignoring SIGTERM"}]}}'
    while :; do sleep 0.2; done
    ;;
  *MODE_ECHO_PROMPT*)
    printf '%s' "$prompt" > received-prompt.txt
    exit 0
    ;;
  *MODE_ENV*)
    echo "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":[{\"type\":\"text\",\"text\":\"leak=${FAKE_SECRET_TOKEN:-unset} other=${UNLISTED_VAR:-unset} cwd=$(pwd -P)\"}]}}"
    exit 0
    ;;
esac

echo '{"type":"tool_execution_start","toolCallId":"c1","toolName":"write","args":{"path":"agent-output.txt"}}'
printf 'written by fake agent\n' > agent-output.txt
git add agent-output.txt
git -c user.email=fake@example.com -c user.name="Fake Agent" commit -q -m "feat: fake agent change"
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done: committed agent-output.txt"}]}}'
exit 0
