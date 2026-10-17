// A durable inbox, keyed by queue task ID rather than attachment path.
function collectResponses(existing, tasks, host) {
  const responses = { ...existing };
  const added = [];
  for (const task of tasks) {
    if (task.from !== host || !['done', 'error'].includes(task.status)) continue;
    if (!task.id || responses[task.id]) continue;
    const response = {
      id: task.id, project: task.project || '', message: task.message || '',
      name: (task.filenames || []).join(', ') || 'Text prompt',
      status: task.status, result: task.result || '',
      processedBy: task.processedBy || '',
      doneAt: task.doneAt || new Date().toISOString(), read: false,
    };
    responses[task.id] = response;
    added.push(response);
  }
  return { responses, added };
}

module.exports = { collectResponses };
