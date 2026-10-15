(() => {
  'use strict';

  const $ = (id) => document.getElementById(id);
  const number = new Intl.NumberFormat('en-US');
  const integer = (value) => Number.isFinite(value) ? number.format(value) : 'n/a';
  const duration = (seconds) => {
    const value = Math.max(0, Math.floor(Number(seconds) || 0));
    return `${Math.floor(value / 60)}m ${value % 60}s`;
  };
  const timestamp = (value) => {
    const parsed = Date.parse(value);
    return Number.isFinite(parsed) ? parsed : null;
  };
  const elapsed = (startedAt, now) => {
    const started = timestamp(startedAt);
    return started === null ? 0 : Math.max(0, (now - started) / 1000);
  };
  const field = (row, name) => row.querySelector(`[data-field="${name}"]`);
  let view = null;
  let receivedAt = Date.now();
  let requestInFlight = false;
  let snapshotPending = false;
  let snapshotVersion = 0;
  const sessionRows = new Map();
  const retryRows = new Map();

  function reconcileRows(container, rows) {
    const wanted = new Set(rows);
    for (const child of Array.from(container.children)) {
      if (!wanted.has(child)) child.remove();
    }
    rows.forEach((row, index) => {
      if (container.children[index] !== row) container.insertBefore(row, container.children[index] || null);
    });
  }

  function connection(state, label) {
    $('connection').className = `status-badge status-badge-${state}`;
    $('connection-label').textContent = label;
  }

  function unavailable(message) {
    $('snapshot-error').hidden = false;
    $('error-message').textContent = message;
  }

  function issueFields(row, issue) {
    field(row, 'identifier').textContent = issue.issue_identifier;
    // Encode the whole identifier; GitHub/Forgejo identifiers can contain / and #.
    field(row, 'details').href = `/api/v1/${encodeURIComponent(issue.issue_identifier)}`;
  }

  function stateClass(state) {
    const name = String(state || '').toLowerCase();
    if (/blocked|error|failed/.test(name)) return 'state-badge-danger';
    if (/progress|running|active/.test(name)) return 'state-badge-active';
    if (/todo|queued|pending|retry/.test(name)) return 'state-badge-warning';
    return '';
  }

  function updateTimers() {
    if (!view) return;
    // Rust snapshots already include active-session runtime. Add only time
    // since receipt, once per active worker, rather than counting it twice.
    const delta = Math.max(0, (Date.now() - receivedAt) / 1000);
    $('total-runtime').textContent = duration((view.agent_totals.seconds_running || 0) + delta * view.running.length);
    const snapshotTime = timestamp(view.generated_at);
    const now = (snapshotTime ?? receivedAt) + delta * 1000;
    for (const [id, row] of sessionRows) {
      const issue = view.running.find((item) => item.issue_id === id);
      if (!issue) continue;
      const runtime = duration(elapsed(issue.started_at, now));
      field(row, 'runtime').textContent = issue.turn_count > 0 ? `${runtime} / ${integer(issue.turn_count)}` : runtime;
    }
  }

  async function copySession(button, sessionId) {
    button.disabled = true;
    try {
      await navigator.clipboard.writeText(sessionId);
      if (button.dataset.sessionId === sessionId) button.textContent = 'Copied';
    } catch (_) {
      if (button.dataset.sessionId === sessionId) button.textContent = 'Copy failed';
    } finally {
      button.disabled = false;
      setTimeout(() => { button.textContent = 'Copy ID'; }, 1500);
    }
  }

  function render(next) {
    if (!next || !next.counts || !next.agent_totals || !Array.isArray(next.running) || !Array.isArray(next.retrying)) {
      throw new Error('The runtime snapshot is unavailable or incomplete. Retrying automatically.');
    }
    view = next;
    receivedAt = Date.now();
    snapshotVersion += 1;
    $('snapshot-error').hidden = true;
    $('running-count').textContent = integer(view.counts.running);
    $('retrying-count').textContent = integer(view.counts.retrying);
    const totals = view.agent_totals;
    $('total-tokens').textContent = integer(totals.total_tokens);
    $('token-detail').textContent = `In ${integer(totals.input_tokens)} / Out ${integer(totals.output_tokens)}`;
    $('snapshot-time').textContent = `Snapshot ${view.generated_at}`;
    $('rate-limits').textContent = view.rate_limits == null ? 'No rate-limit snapshot available.' : JSON.stringify(view.rate_limits, null, 2);

    const running = [];
    const activeIds = new Set(view.running.map((issue) => issue.issue_id));
    for (const id of sessionRows.keys()) {
      if (!activeIds.has(id)) sessionRows.delete(id);
    }
    for (const issue of view.running) {
      const row = sessionRows.get(issue.issue_id) || $('running-row').content.firstElementChild.cloneNode(true);
      issueFields(row, issue);
      const state = field(row, 'state');
      state.textContent = issue.state;
      state.className = `state-badge ${stateClass(issue.state)}`;
      const copy = field(row, 'copy');
      if (!sessionRows.has(issue.issue_id)) {
        copy.addEventListener('click', () => copySession(copy, copy.dataset.sessionId));
      }
      if (copy.dataset.sessionId !== issue.session_id) {
        copy.dataset.sessionId = issue.session_id || '';
        copy.textContent = 'Copy ID';
      }
      copy.hidden = !issue.session_id;
      field(row, 'no-session').hidden = Boolean(issue.session_id);
      copy.setAttribute('aria-label', `Copy session ID for ${issue.issue_identifier}`);
      const message = issue.last_message || issue.last_event || 'n/a';
      field(row, 'message').textContent = message;
      field(row, 'message').title = message;
      field(row, 'event').textContent = `${issue.last_event || 'n/a'}${issue.last_event_at ? ` · ${issue.last_event_at}` : ''}`;
      const tokens = issue.tokens || {};
      field(row, 'tokens').textContent = `Total: ${integer(tokens.total_tokens)}`;
      field(row, 'token-detail').textContent = `In ${integer(tokens.input_tokens)} / Out ${integer(tokens.output_tokens)}`;
      sessionRows.set(issue.issue_id, row);
      running.push(row);
    }
    reconcileRows($('running-rows'), running);
    $('running-table').hidden = !view.running.length;
    $('running-empty').hidden = Boolean(view.running.length);
    $('running-empty').textContent = 'No active sessions.';

    const retrying = [];
    const retryIds = new Set(view.retrying.map((issue) => issue.issue_id));
    for (const id of retryRows.keys()) {
      if (!retryIds.has(id)) retryRows.delete(id);
    }
    for (const issue of view.retrying) {
      const row = retryRows.get(issue.issue_id) || $('retrying-row').content.firstElementChild.cloneNode(true);
      issueFields(row, issue);
      field(row, 'attempt').textContent = integer(issue.attempt);
      field(row, 'due').textContent = issue.due_at || 'n/a';
      field(row, 'error').textContent = issue.error || 'n/a';
      retryRows.set(issue.issue_id, row);
      retrying.push(row);
    }
    reconcileRows($('retrying-rows'), retrying);
    $('retrying-table').hidden = !view.retrying.length;
    $('retrying-empty').hidden = Boolean(view.retrying.length);
    $('retrying-empty').textContent = 'No issues are currently backing off.';
    updateTimers();
  }

  async function snapshot() {
    if (requestInFlight) { snapshotPending = true; return; }
    requestInFlight = true;
    const version = snapshotVersion;
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), 10000);
    try {
      const response = await fetch('/api/v1/state', { signal: controller.signal, cache: 'no-store' });
      if (!response.ok) throw new Error(`Snapshot request failed (${response.status}). Retrying automatically.`);
      const next = await response.json();
      // A newer SSE snapshot wins over an earlier, slower REST request.
      if (version === snapshotVersion) render(next);
    } catch (error) {
      if (version === snapshotVersion) unavailable(error.name === 'AbortError' ? 'Snapshot request timed out. Retrying automatically.' : error.message);
    } finally {
      clearTimeout(timeout);
      requestInFlight = false;
      if (snapshotPending) { snapshotPending = false; snapshot(); }
    }
  }

  function log(kind, data, lagged = false) {
    const entry = document.createElement('div');
    entry.className = `activity-entry${lagged ? ' lag' : ''}`;
    const metadata = document.createElement('span');
    metadata.className = 'muted event-meta mono';
    metadata.textContent = `${data.timestamp || ''} · ${data.issue_identifier || 'System'} · ${kind}`;
    const message = document.createElement('span');
    message.textContent = data.message || kind.replaceAll('_', ' ');
    entry.append(metadata, message);
    $('events').prepend(entry);
    $('events-empty').hidden = true;
    while ($('events').children.length > 200) $('events').lastElementChild.remove();
  }

  try {
    const initial = JSON.parse($('initial-state').textContent);
    if (initial) render(initial);
  } catch (_) {
    unavailable('The initial snapshot could not be read. Retrying automatically.');
  }

  const events = new EventSource('/api/v1/events');
  events.onopen = () => { connection('live', 'Live'); snapshot(); };
  events.onerror = () => {
    connection(events.readyState === EventSource.CLOSED ? 'offline' : 'reconnecting', events.readyState === EventSource.CLOSED ? 'Offline' : 'Reconnecting…');
  };
  events.addEventListener('snapshot', (event) => {
    try { render(JSON.parse(event.data)); }
    catch (_) { unavailable('A live snapshot could not be read. Fetching a fresh snapshot.'); snapshot(); }
  });
  events.addEventListener('lagged', (event) => {
    log('lagged', { message: `Some live updates were missed. Refreshing the snapshot. ${event.data}` }, true);
    snapshot();
  });
  ['session_started', 'turn_completed', 'turn_failed', 'turn_cancelled', 'notification', 'approval_auto_approved', 'tool_call_completed', 'tool_call_failed', 'unsupported_tool_call', 'malformed', 'other_message', 'startup_failed', 'turn_input_required'].forEach((kind) => {
    events.addEventListener(kind, (event) => {
      try { log(kind, JSON.parse(event.data)); } catch (_) { /* Keep the live stream usable after a malformed event. */ }
      snapshot();
    });
  });
  setInterval(updateTimers, 1000);
  // Periodic recovery also refreshes quiet runs and retries after API failures.
  setInterval(snapshot, 15000);
  window.addEventListener('pagehide', () => events.close());
})();
