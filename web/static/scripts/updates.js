// web/static/scripts/updates.js
// The Updates page (docs/updates.md): which version and slot runs, finding a new version on GitHub or taking one from a file, writing it
// into the other slot, starting it on trial and keeping it (or going back). All state is on the router (GET /api/v1/updates); this file
// shows it and sends the choices. watch() runs on every page: the mark in the menu and the bar while a version is on trial.

window.UpdatesModule = (function () {
    let st = null;                 // the state from the router
    let lost = false;              // the router cannot be reached (it is restarting, or the network is down)
    let restarting = null;         // { since, kind } while the router restarts into another version
    let confirm = null;            // 'restart' | 'install' | 'go-back' | 'keep' until clicked again
    let busy = null;               // what is being asked of the router now
    let error = null;
    let up = { file: null, manifest: null, error: null, sending: false, done: 0, total: 0 };
    let pollTimer = null;
    let wired = false;
    let watching = false;
    let leftAt = 0;                // when the trial countdown was read, by this browser's clock
    let src = { prereleases: false, auto_check: true, dirty: false };

    const $ = (id) => document.getElementById(id);
    const esc = (v) => escapeHtml(v);
    const dateOf = (s) => { const d = new Date(s); return isNaN(d) ? '' : d.toLocaleString(undefined, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' }); };
    const mb = (b) => (b >= 1048576 ? (b / 1048576).toFixed(b >= 1.0e8 ? 0 : 1) + ' MB' : Math.max(1, Math.round(b / 1024)) + ' kB');
    const ACTION = { installed: 'Installed', trial: 'Started on trial', kept: 'Kept', reverted: 'Went back', failed: 'Failed' };

    // ---------------------------------------------------------------- talking to the router

    async function errorText(res, fallback) {
        if (!res) return 'The router did not answer. Try again.';
        try { const e = await res.json(); return e.error || fallback; } catch (e) { return fallback; }
    }

    async function load() {
        const res = await apiFetch('/api/v1/updates');
        if (!res || !res.ok) { lost = true; return false; }
        lost = false;
        take(await res.json());
        return true;
    }

    function take(state) {
        st = state;
        leftAt = Date.now();
        mark(state);
        bar(state);
        if (!src.dirty) src = { prereleases: !!st.settings.prereleases, auto_check: !!st.settings.auto_check, dirty: false };
    }

    async function act(name, path, init, fallback) {
        busy = name; error = null; confirm = null; render();
        const res = await apiFetch(path, init);
        busy = null;
        if (!res || !res.ok) { error = await errorText(res, fallback); render(); return null; }
        const body = await res.json();
        take(body);
        render();
        return body;
    }

    const post = (name, path, body) => act(name, path, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body || {}) }, 'That did not work.');

    function check() { return act('check', '/api/v1/updates/check', { method: 'POST' }, 'The check did not work.').then(schedule); }
    function install(version) { return post('install', '/api/v1/updates/install', { version }).then(schedule); }
    function cancel() { return act('cancel', '/api/v1/updates/cancel', { method: 'POST' }, 'That did not work.'); }
    function keep() { return post('keep', '/api/v1/updates/keep'); }

    async function restart(kind, path) {
        const body = await post(kind, path);
        if (body) { restarting = { since: Date.now(), kind }; lost = true; schedule(); render(); }
    }

    async function saveSource() {
        const body = { prereleases: src.prereleases, auto_check: src.auto_check };
        src.dirty = false;
        await act('source', '/api/v1/updates/settings', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) }, 'The update source was not saved.');
    }

    // ---------------------------------------------------------------- reading an update file here, before it is sent

    // A bundle is a tar archive whose first entry is manifest.json: the header (512 bytes) and then the JSON.
    async function readManifest(file) {
        const head = new Uint8Array(await file.slice(0, 512 + 65536).arrayBuffer());
        const text = (a, b) => new TextDecoder().decode(head.subarray(a, b)).replace(/\0.*$/s, '');
        if (head.length < 512 || text(0, 100) !== 'manifest.json') throw new Error('This does not look like a PeakRoute update file.');
        const size = parseInt(text(124, 136).trim(), 8);
        if (!(size > 0 && size <= 65536) || 512 + size > head.length) throw new Error('This does not look like a PeakRoute update file.');
        const m = JSON.parse(new TextDecoder().decode(head.subarray(512, 512 + size)));
        if (!m || m.format !== 1 || !m.version) throw new Error('This update file is of a kind this version does not know.');
        return m;
    }

    async function chooseFile(file) {
        up = { file, manifest: null, error: null, sending: false, done: 0, total: 0 };
        if (file) {
            try { up.manifest = await readManifest(file); } catch (e) { up.error = e.message || 'This file cannot be read.'; }
        }
        render();
    }

    function send() {
        const f = up.file;
        if (!f || !up.manifest) return;
        up.sending = true; up.done = 0; up.total = f.size; error = null; confirm = null; render();
        const xhr = new XMLHttpRequest();
        xhr.open('POST', '/api/v1/updates/upload');
        const token = document.querySelector('meta[name="csrf-token"]');
        if (token) xhr.setRequestHeader('X-CSRF-Token', token.content);
        xhr.setRequestHeader('Content-Type', 'application/octet-stream');
        xhr.upload.onprogress = (e) => { if (e.lengthComputable) { up.done = e.loaded; up.total = e.total; patchProgress(); } };
        xhr.onload = () => {
            up.sending = false;
            let body = null;
            try { body = JSON.parse(xhr.responseText); } catch (e) { /* not JSON */ }
            if (xhr.status === 401) { window.location.href = '/login'; return; }
            if (xhr.status >= 200 && xhr.status < 300 && body) { take(body); up = { file: null, manifest: null, error: null, sending: false, done: 0, total: 0 }; }
            else { error = (body && body.error) || 'The update could not be installed.'; }
            render();
        };
        xhr.onerror = () => { up.sending = false; error = 'The router did not answer. The upload was cut off; try again.'; render(); };
        xhr.send(f);
        schedule(); // the router shows what it is writing
    }

    // ---------------------------------------------------------------- what to show

    function trialLeft() {
        if (!st || !st.on_trial) return 0;
        return Math.max(0, (st.trial_seconds_left || 0) - Math.floor((Date.now() - leftAt) / 1000));
    }
    const clock = (s) => Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');

    function btn(id, label, cls, disabled) { return `<button type="button" class="cfg-btn ${cls || 'cfg-btn--ghost'}" id="${id}"${disabled ? ' disabled' : ''}>${label}</button>`; }
    function sure(key, label, again) { return confirm === key ? again : label; }

    function versionsHtml() {
        const r = st.running;
        const kept = r.slot && r.kept ? (r.slot === r.kept ? 'this slot' : 'the other slot') : '';
        const other = st.other_slot
            ? (st.other_version ? esc(st.other_version) : '<span class="form-hint">nothing yet</span>') : '';
        return `<section class="card" id="up-versions">
            <div class="net-card-head"><h2>This router</h2>${st.on_trial ? '<span class="tag tag-draft">on trial</span>' : '<span class="tag tag-ok">kept</span>'}</div>
            <dl class="inet-live">
                <div><dt>PeakRoute OS</dt><dd>${esc(r.version)}</dd></div>
                <div><dt>Built</dt><dd>${esc(r.built ? dateOf(r.built) : '–')}</dd></div>
                <div><dt>Debian</dt><dd>${esc(r.debian || '–')}</dd></div>
                <div><dt>Kernel</dt><dd>${esc(r.kernel || '–')}</dd></div>
                <div><dt>Processor</dt><dd>${esc(r.arch || '–')}</dd></div>
                ${r.slot ? `<div><dt>Running from</dt><dd>slot ${esc(r.slot.toUpperCase())}</dd></div>
                <div><dt>Starts from</dt><dd>slot ${esc((r.kept || r.slot).toUpperCase())}${kept === 'this slot' ? '' : ''}</dd></div>
                <div><dt>Other slot (${esc(st.other_slot.toUpperCase())})</dt><dd>${other}</dd></div>` : ''}
            </dl>
            ${!st.persistent ? '<div class="form-message" role="alert">This router could not use its data partition at the last start, so its settings are kept in memory only and are lost at a restart. Updating is not safe like this.</div>' : ''}
        </section>`;
    }

    function trialHtml() {
        const left = trialLeft();
        return `<section class="card" id="up-trial">
            <div class="net-card-head"><h2>${esc(st.running.version)} is on trial</h2><span class="tag tag-draft">${left > 0 ? clock(left) + ' left' : 'time is up'}</span></div>
            <p class="form-hint">This is the new version, started once. If everything works (the internet, the Wi-Fi, the pages), <strong>keep it</strong>. If you do nothing, or the router fails to start, it goes back to the version before by itself ${left > 0 ? 'in <strong id="up-left">' + clock(left) + '</strong>' : 'about now'}.</p>
            ${error ? `<div class="form-message" role="alert">${esc(error)}</div>` : ''}
            <div class="form-actions">${btn('up-go-back', sure('go-back', 'Go back to the old version', 'Click again to restart'), 'cfg-btn--danger', busy)}
                ${btn('up-keep', busy === 'keep' ? 'Keeping…' : 'Keep this version', 'cfg-btn--primary', busy)}</div>
        </section>`;
    }

    function stagedHtml() {
        const s = st.staged;
        return `<section class="card" id="up-staged">
            <div class="net-card-head"><h2>${esc(s.version)} is ready</h2><span class="tag tag-ok">written</span></div>
            <p class="form-hint">It is written into slot ${esc(s.slot.toUpperCase())} and checked. Starting it restarts the router: a minute or two without internet. It starts <strong>on trial</strong>: you have ten minutes to keep it, and otherwise the router goes back to ${esc(st.running.version)} by itself. A copy of the settings is kept first.</p>
            ${s.notes ? `<p class="form-hint">${esc(s.notes)}</p>` : ''}
            ${!st.editable ? '<p class="form-hint net-warn">A change is being applied or waits for confirmation: confirm or revert it first.</p>' : ''}
            ${error ? `<div class="form-message" role="alert">${esc(error)}</div>` : ''}
            <div class="form-actions">${btn('up-restart', busy === 'restart' ? 'Restarting…' : sure('restart', 'Restart into ' + esc(s.version), 'Click again to restart the router'), 'cfg-btn--primary', busy || !st.editable)}</div>
        </section>`;
    }

    function jobHtml() {
        const j = st.job;
        if (!j) return '';
        const running = j.phase === 'starting' || j.phase === 'writing' || j.phase === 'checking';
        const pct = j.total > 0 ? Math.min(100, Math.floor(j.done * 100 / j.total)) : 0;
        const label = { starting: 'Starting…', writing: j.source === 'github' ? 'Downloading and writing' : 'Receiving and writing', checking: 'Checking the new system…', done: 'Written', failed: 'Failed' }[j.phase] || j.phase;
        if (!running && j.phase === 'done') return '';
        return `<div class="up-job">
            <p class="form-hint"><strong>${esc(label)}</strong>${j.version ? ' ' + esc(j.version) : ''}${running && j.total > 0 && j.phase === 'writing' ? ' — ' + mb(j.done) + ' of ' + mb(j.total) : ''}</p>
            ${running ? `<div class="progress-track"><div class="progress-fill fill-blue" id="up-bar" style="width:${j.phase === 'checking' ? 100 : pct}%"></div></div>` : ''}
            ${j.phase === 'failed' ? `<div class="form-message" role="alert">${esc(j.error || 'The update failed.')} The version that is running was not touched.</div>` : ''}
        </div>`;
    }

    function foundHtml() {
        const c = st.check;
        if (!c) return '<p class="form-hint">Nothing has been checked yet.</p>';
        const when = `<p class="form-hint">Last checked ${esc(dateOf(c.time))}.</p>`;
        if (c.error) return `<div class="form-message" role="alert">${esc(c.error)}</div>${when}`;
        const r = c.release;
        if (!r.newer) return `<p><span class="tag tag-ok">up to date</span> The newest release (${esc(r.version)}) is the one running or older.</p>${when}`;
        const jobRunning = st.job && ['starting', 'writing', 'checking'].includes(st.job.phase);
        return `<div class="up-found"><p><strong>${esc(r.version)}</strong>${r.prerelease ? ' <span class="tag tag-draft">pre-release</span>' : ''} is available${r.published && r.published.indexOf('0001') !== 0 ? ', published ' + esc(dateOf(r.published)) : ''}${r.size ? ' (' + mb(r.size) + ')' : ''}.</p>
            ${r.notes ? `<pre class="up-notes">${esc(r.notes)}</pre>` : ''}
            <div class="form-actions">${btn('up-install', busy === 'install' ? 'Starting…' : 'Download and install ' + esc(r.version), 'cfg-btn--primary', busy || jobRunning || !st.editable || st.on_trial)}</div></div>${when}`;
    }

    function updatesHtml() {
        const jobRunning = st.job && ['starting', 'writing', 'checking'].includes(st.job.phase);
        const source = st.has_source;
        return `<section class="card" id="up-find">
            <div class="net-card-head"><h2>Check for updates</h2>${st.settings.prereleases ? '<span class="tag tag-draft">pre-releases included</span>' : ''}</div>
            ${source ? '' : '<p class="form-hint">This build does not say where its updates are published. Use an update file instead.</p>'}
            ${foundHtml()}
            ${jobHtml()}
            ${st.on_trial ? '<p class="form-hint">Keep the version on trial or go back before installing another.</p>' : ''}
            ${error && !st.on_trial && !st.staged && !(st.job && st.job.error === error) ? `<div class="form-message" role="alert">${esc(error)}</div>` : ''}
            <div class="form-actions">${jobRunning && st.job.source === 'github' ? btn('up-cancel', 'Cancel', 'cfg-btn--ghost') : ''}
                ${btn('up-check', busy === 'check' ? 'Asking GitHub…' : 'Check for updates', 'cfg-btn--ghost', busy || jobRunning || !source)}</div>
        </section>`;
    }

    function uploadHtml() {
        const m = up.manifest;
        const jobRunning = st.job && ['starting', 'writing', 'checking'].includes(st.job.phase);
        let detail = '';
        if (up.error) detail = `<div class="form-message" role="alert">${esc(up.error)}</div>`;
        else if (m) {
            const same = m.version === st.running.version;
            detail = `<div class="up-found"><p><strong>${esc(m.version)}</strong>, built ${esc(dateOf(m.built))}${same ? ' <span class="tag tag-draft">the version that is running</span>' : ''}.</p>
                ${m.notes ? `<pre class="up-notes">${esc(m.notes)}</pre>` : ''}
                ${up.sending ? `<p class="form-hint"><strong>Sending</strong> ${mb(up.done)} of ${mb(up.total)}</p><div class="progress-track"><div class="progress-fill fill-blue" id="up-bar" style="width:${up.total ? Math.floor(up.done * 100 / up.total) : 0}%"></div></div>` : ''}
                <div class="form-actions">${btn('up-send', up.sending ? 'Installing…' : 'Install ' + esc(m.version), 'cfg-btn--primary', up.sending || jobRunning || busy || !st.editable || st.on_trial)}</div></div>`;
        }
        return `<section class="card" id="up-upload">
            <div class="net-card-head"><h2>Install from a file</h2></div>
            <p class="form-hint">An update file (<code>peakroute-os-….update</code>) made by the build, or downloaded from a release. It is written into the other slot like a download; the version that is running is not touched.</p>
            <div class="form-grid"><div class="form-field"><label for="up-file">Update file</label><input type="file" id="up-file" accept=".update,application/octet-stream"${up.sending ? ' disabled' : ''}></div></div>
            ${detail}
        </section>`;
    }

    function sourceHtml() {
        return `<section class="card" id="up-source">
            <div class="net-card-head"><h2>Update settings</h2></div>
            <div class="net-access"><label class="toggle toggle--block"><input type="checkbox" id="up-pre"${src.prereleases ? ' checked' : ''}><span><strong>Include pre-releases</strong><span class="form-hint">Alpha and beta versions. Off: only full releases are offered.</span></span></label>
                <label class="toggle toggle--block"><input type="checkbox" id="up-auto"${src.auto_check ? ' checked' : ''}><span><strong>Look once a day</strong><span class="form-hint">The router asks GitHub if there is a newer release and says so here. <strong>Nothing is installed by itself.</strong></span></span></label></div>
            <div class="form-actions">${btn('up-save', busy === 'source' ? 'Saving…' : 'Save', 'cfg-btn--primary', busy || !src.dirty)}</div>
        </section>`;
    }

    function historyHtml() {
        if (!st.history.length) return '';
        const rows = st.history.slice(0, 12).map(h => `<tr><td>${esc(dateOf(h.time))}</td><td>${esc(ACTION[h.action] || h.action)}</td><td>${esc(h.version || '')}</td><td>${esc(h.detail || '')}</td></tr>`).join('');
        return `<section class="card"><div class="net-card-head"><h2>History</h2></div><div class="table-wrap"><table class="data-table"><thead><tr><th>When</th><th>What</th><th>Version</th><th>Details</th></tr></thead><tbody>${rows}</tbody></table></div></section>`;
    }

    function goBackHtml() {
        // a kept version with an earlier one in the other slot
        if (st.on_trial || !st.supported || !st.other_version || st.staged) return '';
        return `<section class="card" id="up-back">
            <div class="net-card-head"><h2>Go back</h2></div>
            <p class="form-hint">The other slot still holds <strong>${esc(st.other_version)}</strong>. Starting it restarts the router and runs it on trial, like an update.</p>
            ${error ? `<div class="form-message" role="alert">${esc(error)}</div>` : ''}
            <div class="form-actions">${btn('up-back-go', sure('go-back', 'Start ' + esc(st.other_version), 'Click again to restart the router'), 'cfg-btn--ghost', busy || !st.editable)}</div></section>`;
    }

    function restartingHtml() {
        const secs = Math.floor((Date.now() - restarting.since) / 1000);
        return `<section class="card"><div class="net-card-head"><h2>Restarting…</h2></div>
            <p class="form-hint">The router is restarting${restarting.kind === 'restart' ? ' into the new version' : ''}. This takes a minute or two (${secs} s so far). This page comes back by itself; you may have to sign in again.</p>
            <div class="progress-track"><div class="progress-fill fill-blue" style="width:${Math.min(95, 8 + secs / 1.5)}%"></div></div></section>`;
    }

    function render() {
        const body = $('updates-body');
        if (!body) return;
        if (!st) { body.innerHTML = `<section class="card"><p class="form-hint">${lost ? 'The router did not answer.' : 'Loading…'}</p></section>`; return; }
        // the old version answers for a few seconds after the restart was asked for: that is not the new one
        if (restarting && (lost || Date.now() - restarting.since < 15000)) { body.innerHTML = restartingHtml(); return; }
        const parts = [];
        if (!st.supported) {
            parts.push(`<section class="card"><div class="net-card-head"><h2>Updates are not available</h2></div><p class="form-hint">${esc(st.problem)}</p></section>`);
            parts.push(versionsHtml());
        } else {
            if (st.on_trial) parts.push(trialHtml());
            if (st.staged && !st.on_trial) parts.push(stagedHtml());
            parts.push(versionsHtml());
            parts.push(updatesHtml(), uploadHtml(), goBackHtml(), sourceHtml());
        }
        parts.push(historyHtml());
        body.innerHTML = parts.join('');
    }

    function patchProgress() {
        const bar = $('up-bar');
        if (bar && up.sending && up.total) bar.style.width = Math.floor(up.done * 100 / up.total) + '%';
    }

    // ---------------------------------------------------------------- polling

    function schedule() {
        clearTimeout(pollTimer);
        pollTimer = null;
        if (!$('updates-body') || !document.getElementById('view-updates') || document.getElementById('view-updates').classList.contains('hidden')) return;
        const jobRunning = st && st.job && ['starting', 'writing', 'checking'].includes(st.job.phase);
        let wait = 0;
        if (restarting || lost) wait = 2000;
        else if (jobRunning || up.sending) wait = 1000;
        else if (st && st.on_trial) wait = 1000;
        if (!wait) return;
        pollTimer = setTimeout(async () => {
            const ok = await load();
            if (ok && restarting && Date.now() - restarting.since > 15000) restarting = null;
            if (restarting && Date.now() - restarting.since > 600000) restarting = null;
            const t = $('up-left');
            if (t) { const s = trialLeft(); t.textContent = clock(s); }
            if (!(up.sending && !ok)) render();
            schedule();
        }, wait);
    }

    // ---------------------------------------------------------------- events

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('updates-body');
        body.addEventListener('click', (e) => {
            const b = e.target.closest('button');
            if (!b || b.disabled) return;
            switch (b.id) {
                case 'up-check': check(); break;
                case 'up-install': install(st.check.release.version); break;
                case 'up-cancel': cancel(); break;
                case 'up-send': send(); break;
                case 'up-save': saveSource(); break;
                case 'up-keep': keep(); break;
                case 'up-restart':
                    if (confirm !== 'restart') { confirm = 'restart'; render(); } else { confirm = null; restart('restart', '/api/v1/updates/restart'); }
                    break;
                case 'up-go-back':
                    if (confirm !== 'go-back') { confirm = 'go-back'; render(); } else { confirm = null; restart('revert', '/api/v1/updates/revert'); }
                    break;
                case 'up-back-go':
                    if (confirm !== 'go-back') { confirm = 'go-back'; render(); } else { confirm = null; restart('revert', '/api/v1/updates/revert'); }
                    break;
            }
        });
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (el.id === 'up-file') chooseFile(el.files[0] || null);
            else if (el.id === 'up-pre') { src.prereleases = el.checked; src.dirty = true; $('up-save').disabled = false; }
            else if (el.id === 'up-auto') { src.auto_check = el.checked; src.dirty = true; $('up-save').disabled = false; }
        });
    }

    function init() {
        wire();
        error = null; confirm = null;
        render();
        load().then(() => { render(); schedule(); });
        return () => { clearTimeout(pollTimer); pollTimer = null; };
    }

    // ---------------------------------------------------------------- on every page: the menu mark and the bar for a version on trial

    function bar(state) {
        const el = $('update-banner');
        if (!el) return;
        if (!state || !state.on_trial) { el.classList.add('hidden'); el.innerHTML = ''; return; }
        const left = Math.max(0, (state.trial_seconds_left || 0));
        el.classList.remove('hidden');
        el.innerHTML = `<div class="config-bar config-bar--confirm"><div class="config-bar-text"><strong>${esc(state.running.version)} is on trial.</strong> Keep it if everything works, or the router goes back to the old version${left > 0 ? ' in about ' + clock(left) : ' about now'}.</div>
            <div class="config-bar-actions"><button type="button" class="cfg-btn cfg-btn--primary" id="up-bar-open">Open Updates</button></div></div>`;
        const open = $('up-bar-open');
        if (open) open.onclick = () => loadView('updates');
    }

    function mark(state) {
        const link = document.querySelector('.nav-item[data-view="updates"]');
        if (!link) return;
        const dot = state && (state.available || state.on_trial);
        link.classList.toggle('nav-dot', !!dot);
        link.title = state && state.on_trial ? 'A version is on trial' : (state && state.available ? 'A new version is available' : '');
    }

    async function glance() {
        const res = await fetch('/api/v1/updates', { credentials: 'same-origin' }).catch(() => null);
        if (!res || !res.ok) return null;
        const state = await res.json();
        mark(state);
        bar(state);
        return state;
    }

    function watch() {
        if (watching) return;
        watching = true;
        const tick = async () => {
            const state = await glance();
            setTimeout(tick, state && state.on_trial ? 5000 : 120000);
        };
        tick();
    }

    return { init, watch };
})();
