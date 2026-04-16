export interface PromptOptions {
  screenshotPath: string;
  repo: string;
  message?: string;
}

/** Prompt for direct local execution (no agent-queue) */
export function buildPrompt(opts: PromptOptions): string {
  const isUrl =
    opts.repo.startsWith("http") ||
    opts.repo.startsWith("git@") ||
    /^[\w-]+\/[\w.-]+$/.test(opts.repo);

  const timestamp = Date.now();
  const branch = `screenshot-fix/${timestamp}`;

  const repoSetup = isUrl
    ? `Clone the repo: git clone https://github.com/${opts.repo.replace(/^https:\/\/github\.com\//, "")} /tmp/screenshot-agent-${timestamp}
cd /tmp/screenshot-agent-${timestamp}`
    : `cd ${opts.repo}`;

  const userContext = opts.message
    ? `\n\nAdditional context from the user: "${opts.message}"`
    : "";

  return `You are a screenshot-driven code fix agent. Your job is to analyze a screenshot, understand the problem, and fix it in a target repo.

## Step 1: Analyze the screenshot

Read the file at ${opts.screenshotPath} using the Read tool. This is a screenshot showing a bug, UI issue, error, or desired change.${userContext}

Describe what you see and identify exactly what needs to change.

## Step 2: Set up the repo

${repoSetup}

Create a new branch:
git checkout -b ${branch}

## Step 3: Find and fix the issue

Based on your analysis of the screenshot, explore the codebase to find the relevant source files. Make the necessary code changes to fix the identified issue. Be surgical — only change what is needed.

## Step 4: Commit and push

Stage the changed files (by name, not git add -A), write a clear commit message describing what the screenshot showed and what you fixed, and push the branch:
git push -u origin ${branch}

## Step 5: Create and merge PR

Create a pull request:
gh pr create --title "<concise description of the fix>" --body "## Screenshot analysis
<what you saw in the screenshot>

## Changes made
<what you changed and why>

---
Automated fix by screenshot-agent"

Then merge it:
gh pr merge --squash --auto

If the merge fails (e.g. merge conflicts, required checks), report the PR URL so the user can handle it manually.

## Important

- Do NOT hallucinate file contents — always read files before editing them.
- If the screenshot is unclear or you cannot determine what to fix, explain what you see and stop.
- If the repo requires a build step, run it after your changes to verify they compile.
`;
}

export interface WorkerPromptOptions {
  agentId: string;
  project: string;
  repoUrl: string;
  cloneDir: string;
  branch: string;
  aqScripts: string;
  screenshotQueueDir: string;
  resultsDir: string;
}

/** Prompt for agent-queue worker: claim → read screenshot → fix → merge → complete → loop */
export function buildWorkerPrompt(opts: WorkerPromptOptions): string {
  return `You are a screenshot-driven code fix worker agent (${opts.agentId}).
You process screenshot tasks from the agent-queue, fixing code issues shown in screenshots.

## Environment

- Agent ID: ${opts.agentId}
- Project: ${opts.project}
- Working directory: ${opts.cloneDir}
- Branch: ${opts.branch}
- Agent-queue scripts: ${opts.aqScripts}
- Screenshot tasks dir: ${opts.screenshotQueueDir}
- Results dir: ${opts.resultsDir}

## Worker loop

Run this loop until there are no more items to claim:

### 1. Sync with main

\`\`\`bash
${opts.aqScripts}/agent-queue sync --dir ${opts.cloneDir}
\`\`\`

### 2. Claim next item

\`\`\`bash
ITEM=$(${opts.aqScripts}/agent-queue claim -p ${opts.project} --agent ${opts.agentId})
\`\`\`

If claim returns empty or fails, the queue is empty — exit successfully.

Parse the JSON to get the item ID and title. The title format is:
\`screenshot-fix:<task-id>\` where task-id maps to a directory in the screenshot tasks dir.

### 3. Read the screenshot

Extract the task-id from the item title. Then:
- Read the task file: \`${opts.screenshotQueueDir}/<task-id>/task.json\` to get the repo and message
- Read the screenshot: \`${opts.screenshotQueueDir}/<task-id>/screenshot.*\` (use the Read tool — you can see images)

Analyze the screenshot. Identify what needs to change. Use the message from task.json for additional context.

### 4. Find and fix the issue

You are already in the cloned repo at ${opts.cloneDir} on branch ${opts.branch}.
Explore the codebase, find the relevant files, and make the necessary changes.
Be surgical — only change what is needed.

### 5. Commit

Stage changed files by name (not git add -A). Write a clear commit message:
\`\`\`bash
git add <specific files>
git commit -m "fix: <what the screenshot showed and what you fixed>"
\`\`\`

### 6. Merge via agent-merge (locked merge to main)

\`\`\`bash
${opts.aqScripts}/agent-merge ${opts.branch} --delete-branch
\`\`\`

This acquires an exclusive lock, rebases onto main, merges, and pushes. If it fails (conflict), mark the item as failed (step 6b) and continue.

### 6b. On merge failure

\`\`\`bash
${opts.aqScripts}/agent-queue fail -p ${opts.project} <item-id> --reason "merge conflict"
\`\`\`

Then reset:
\`\`\`bash
git checkout main && git pull --ff-only origin main
git checkout -b ${opts.branch}
\`\`\`

Skip to step 8 (write result as error) and continue the loop.

### 7. Complete the item

\`\`\`bash
${opts.aqScripts}/agent-queue complete -p ${opts.project} <item-id> --branch ${opts.branch}
\`\`\`

### 8. Write result file

Write a JSON result file so the work machine can poll for it:
\`\`\`bash
mkdir -p ${opts.resultsDir}/<task-id>
cat > ${opts.resultsDir}/<task-id>/result.json << 'RESULT'
{
  "status": "success",
  "branch": "${opts.branch}",
  "summary": "<brief description of what you fixed>",
  "timestamp": <current unix timestamp in ms>
}
RESULT
\`\`\`

On failure, write \`"status": "error"\` with the reason in summary.

### 9. Reset branch for next task

\`\`\`bash
git checkout main && git pull --ff-only origin main
BRANCH="${opts.project}-${opts.agentId}-$(date +%s)"
git checkout -b "$BRANCH"
\`\`\`

Then go back to step 1.

## Important

- Do NOT hallucinate file contents — always read files before editing them.
- If a screenshot is unclear, write an error result and move to the next item.
- If the repo requires a build step, run it after changes to verify they compile.
- Always write a result file (success or error) so the work machine gets notified.
- One task at a time. Claim → fix → merge → complete → next.
`;
}
