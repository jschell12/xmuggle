const { test } = require('node:test');
const assert = require('node:assert/strict');
const { collectResponses } = require('./responses');

const completed = { id: 'task-1', from: 'sender', status: 'done', project: 'git@gitlab.com:group/app.git', result: 'Implemented\nMR: https://gitlab.com/group/app/-/merge_requests/1', doneAt: '2026-10-16T19:00:00Z' };

test('collects only terminal responses for this sender', () => {
  const { responses, added } = collectResponses({}, [completed,
    { ...completed, id: 'other', from: 'another-host' },
    { ...completed, id: 'pending', status: 'pending' },
    { ...completed, id: 'processing', status: 'processing' },
    { ...completed, id: 'failed', status: 'error', result: 'Push failed' },
  ], 'sender');
  assert.equal(added.length, 2);
  assert.equal(responses['task-1'].read, false);
  assert.equal(responses['task-1'].result, completed.result);
  assert.equal(responses.failed.status, 'error');
});

test('persisted read state survives polling and restart without duplicate alerts', () => {
  const first = collectResponses({}, [completed], 'sender');
  first.responses['task-1'].read = true;
  const persisted = JSON.parse(JSON.stringify(first.responses));
  const next = collectResponses(persisted, [completed], 'sender');
  assert.equal(next.added.length, 0);
  assert.equal(next.responses['task-1'].read, true);
});

test('responses survive attachment deletion and distinguish repeated submissions', () => {
  const first = collectResponses({}, [{ ...completed, filenames: ['same.txt'] }], 'sender');
  const next = collectResponses(first.responses, [{ ...completed, id: 'task-2', filenames: ['same.txt'] }], 'sender');
  assert.deepEqual(Object.keys(next.responses), ['task-1', 'task-2']);
  assert.equal(next.added.length, 1);
  assert.equal(collectResponses(next.responses, [], 'sender').responses['task-1'].result, completed.result);
});
