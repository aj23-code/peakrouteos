// web/static/scripts/diagnostics.js
// Diagnostics page: small tools for finding out why something does not work: ping and traceroute (run as jobs that are followed
// while they print), a TCP connection test, the DNS lookup (asks the router's own DNS server a question and shows the answer and how
// long it took) and the syslog test (sends a test message to the syslog servers). They change nothing. docs/diagnostics.md.

window.DiagnosticsModule = (function () {
    let lk = { text: '', busy: false, results: [], error: null };   // what was typed, and the latest answers (newest first)
    let sl = { busy: false, results: null, error: null };   // the syslog test
    let wired = false;
    let meta = { sources: [{ id: 'auto', name: 'Automatic (the way the routing table says)', address: '' }], tools: { ping: true, traceroute: true }, max_pings: 20 };
    // ping and traceroute: what was typed, the job that is being followed (or the last one), and the error of the last try
    let jobs = {
        ping: { text: '', count: 4, source: 'auto', job: null, busy: false, error: null },
        traceroute: { text: '', job: null, busy: false, error: null },
    };
    let cx = { host: '', port: '', source: 'auto', busy: false, results: [], error: null };   // the connection test
    let pollTimer = null;

    const $ = (id) => document.getElementById(id);
    const millisText = (ms) => (ms < 1 ? ms.toFixed(2) : ms < 10 ? ms.toFixed(1) : ms.toFixed(0)) + ' ms';


    // ---------------------------------------------------------------- ping and traceroute

    const JOB_TAG = { ok: ['tag tag-ok', 'Finished'], failed: ['tag tag-warn', 'Failed'], stopped: ['tag', 'Stopped'], timeout: ['tag tag-warn', 'Timed out'] };

    function sourceSelect(id, value, lock) {
        return `<select id="${id}" class="role-select" aria-label="Send from"${lock ? ' disabled' : ''}>${meta.sources.map(o =>
            `<option value="${escapeHtml(o.id)}"${o.id === value ? ' selected' : ''}>${escapeHtml(o.name)}${o.address ? ' (' + escapeHtml(o.address) + ')' : ''}</option>`).join('')}</select>`;
    }

    function jobResultHtml(tool) {
        const j = jobs[tool].job;
        if (!j) return '';
        const sum = j.summary;
        let head;
        if (j.running) head = `<span class="tag tag-draft">Running</span> <span class="form-hint">${Math.round(j.elapsed / 1000)} s</span>`;
        else { const [cls, label] = JOB_TAG[j.result] || ['tag', j.result]; head = `<span class="${cls}">${label}</span> <span class="form-hint">${escapeHtml(j.message)}</span>`; }
        const stats = sum ? `<div class="diag-stats"><span><strong>${sum.received}</strong> of ${sum.sent} answered</span><span><strong>${sum.loss.toFixed(sum.loss % 1 ? 1 : 0)}%</strong> lost</span>${sum.received ? `<span>fastest <strong>${millisText(sum.min)}</strong></span><span>average <strong>${millisText(sum.avg)}</strong></span><span>slowest <strong>${millisText(sum.max)}</strong></span>` : ''}</div>` : '';
        return `<div class="diag-job"><div class="diag-job-head">${head}<span class="form-hint diag-cmd">${escapeHtml(j.command)}</span></div>
            ${stats}<pre class="diag-out" id="diag-${tool}-out">${escapeHtml(j.lines.join('\n')) || '<span class="form-hint">Waiting for the first line…</span>'}</pre></div>`;
    }

    function jobCardHtml(tool) {
        const st = jobs[tool];
        const running = st.job && st.job.running;
        const lock = st.busy || running;
        const isPing = tool === 'ping';
        const missing = !meta.tools[tool] ? `<p class="form-hint net-warn">${isPing ? 'ping' : 'tracepath'} is not installed on this router.</p>` : '';
        return `<section class="card" id="diag-${tool}">
            <div class="net-card-head"><h2>${isPing ? 'Ping' : 'Traceroute'}</h2></div>
            <p class="form-hint">${isPing
                ? 'Sends a few small packets to a name or an address and says how many came back and how long they took: the quickest way to find out whether something can be reached. You can send them from the router\'s own address on one of its networks, or from the internet connection, to test that side of the router.'
                : 'Shows the routers a packet passes on its way to a name or an address, one line for each, and how long each took. Use it to see where a connection stops. It leaves the way the routing table says (it cannot be sent from one network\'s address). A line of <code>no reply</code> is a router that does not answer these probes, which is often normal.'}</p>
            ${missing}
            <div class="diag-ask">
                <input type="text" id="diag-${tool}-text" value="${escapeHtml(st.text)}" placeholder="example.com or 1.1.1.1" autocomplete="off" spellcheck="false" autocapitalize="off" aria-label="Name or address"${lock ? ' disabled' : ''}>
                ${isPing ? `<input type="number" id="diag-ping-count" class="diag-count" min="1" max="${meta.max_pings}" value="${st.count}" aria-label="How many"${lock ? ' disabled' : ''} title="How many packets">` : ''}
                ${isPing ? sourceSelect('diag-ping-source', st.source, lock) : ''}
                ${running ? `<button type="button" class="cfg-btn cfg-btn--danger" id="diag-${tool}-stop">Stop</button>`
                    : `<button type="button" class="cfg-btn cfg-btn--primary" id="diag-${tool}-start"${st.busy || !st.text.trim() || !meta.tools[tool] ? ' disabled' : ''}>${st.busy ? 'Starting…' : (isPing ? 'Ping' : 'Trace')}</button>`}
            </div>
            ${st.error ? `<div class="form-message" role="alert">${escapeHtml(st.error)}</div>` : ''}
            ${jobResultHtml(tool)}
        </section>`;
    }

    async function errorOf(res, fallback) {
        let err = {};
        try { err = res ? await res.json() : {}; } catch (e) { /* not JSON */ }
        return err.error || (res ? fallback : 'The router did not answer. Try again.');
    }

    async function startJob(tool) {
        const st = jobs[tool];
        if (st.busy || !st.text.trim()) return;
        st.busy = true; st.error = null; render();
        const body = { tool, target: st.text.trim() };
        if (tool === 'ping') { body.count = parseInt(st.count, 10) || 0; body.source = st.source; }
        const res = await apiFetch('/api/v1/diagnostics/jobs', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        st.busy = false;
        if (res && res.ok) { st.job = await res.json(); schedulePoll(); }
        else st.error = await errorOf(res, 'The test could not be started.');
        render();
    }

    async function stopJob(tool) {
        const j = jobs[tool].job;
        if (!j) return;
        const res = await apiFetch('/api/v1/diagnostics/jobs/' + encodeURIComponent(j.id), { method: 'DELETE' });
        if (res && res.ok) jobs[tool].job = await res.json();
        else jobs[tool].error = await errorOf(res, 'It could not be stopped.');
        render();
    }

    function schedulePoll() {
        if (pollTimer) return;
        pollTimer = setInterval(async () => {
            let any = false;
            for (const tool of Object.keys(jobs)) {
                const j = jobs[tool].job;
                if (!j || !j.running) continue;
                any = true;
                const res = await apiFetch('/api/v1/diagnostics/jobs/' + encodeURIComponent(j.id));
                if (res && res.ok) { jobs[tool].job = await res.json(); }
                else if (res && res.status === 404) { jobs[tool].job = null; }
            }
            if (any) renderJobs();
            else { clearInterval(pollTimer); pollTimer = null; }
        }, 700);
    }

    // while a test runs only its card is drawn again (the other boxes keep what is being typed in them)
    function renderJobs() {
        ['ping', 'traceroute'].forEach(tool => {
            const card = $('diag-' + tool);
            if (!card) return;
            const keep = card.querySelector('.diag-out');
            const atBottom = keep ? keep.scrollTop + keep.clientHeight >= keep.scrollHeight - 4 : true;
            const focused = document.activeElement && card.contains(document.activeElement) ? document.activeElement.id : null;
            const holder = document.createElement('div');
            holder.innerHTML = jobCardHtml(tool);
            card.replaceWith(holder.firstElementChild);
            const out = $('diag-' + tool + '-out');
            if (out && atBottom) out.scrollTop = out.scrollHeight;
            if (focused && $(focused)) $(focused).focus();
        });
    }

    // ---------------------------------------------------------------- the connection test

    function connectHtml() {
        const tagOf = { open: ['tag tag-ok', 'Open'], refused: ['tag tag-warn', 'Refused'], timeout: ['tag tag-warn', 'No answer'], unreachable: ['tag tag-warn', 'Unreachable'], no_name: ['tag tag-warn', 'No such name'], error: ['tag tag-warn', 'Error'] };
        const rows = cx.results.map(r => {
            const [cls, label] = tagOf[r.status] || ['tag', r.status];
            return `<div class="fw-row diag-row"><span class="${cls}">${label}</span>
                <div class="fw-main"><div class="fw-name"><strong>${escapeHtml(r.host)}</strong><span class="cx-port">:${r.port}</span>${r.address && r.address !== r.host ? ` <span class="form-hint">(${escapeHtml(r.address)})</span>` : ''}</div><div class="fw-text form-hint">${escapeHtml(r.message)}</div></div>
                <span class="form-hint diag-ms">${millisText(r.millis)}</span></div>`;
        }).join('');
        return `<section class="card" id="diag-connect">
            <div class="net-card-head"><h2>Connection test</h2></div>
            <p class="form-hint">Tries to open a TCP connection to a port of a name or an address and closes it again, and says whether something answered: <em>open</em> (it did), <em>refused</em> (the machine is there, nothing listens on that port), or <em>no answer</em> (a firewall drops it, or the address is not there). Use it to check a port forward, a server, or a firewall rule. Nothing is sent over the connection.</p>
            <div class="diag-ask">
                <input type="text" id="diag-cx-host" value="${escapeHtml(cx.host)}" placeholder="nas, example.com or 192.168.1.50" autocomplete="off" spellcheck="false" autocapitalize="off" aria-label="Name or address"${cx.busy ? ' disabled' : ''}>
                <input type="number" id="diag-cx-port" class="diag-count diag-port" min="1" max="65535" value="${escapeHtml(String(cx.port))}" placeholder="port" aria-label="Port"${cx.busy ? ' disabled' : ''}>
                ${sourceSelect('diag-cx-source', cx.source, cx.busy)}
                <button type="button" class="cfg-btn cfg-btn--primary" id="diag-cx-go"${cx.busy || !cx.host.trim() || !cx.port ? ' disabled' : ''}>${cx.busy ? 'Trying…' : 'Try'}</button>
            </div>
            ${cx.error ? `<div class="form-message" role="alert">${escapeHtml(cx.error)}</div>` : ''}
            <div class="fw-list">${rows}</div>
        </section>`;
    }

    async function tryConnect() {
        if (cx.busy || !cx.host.trim() || !cx.port) return;
        cx.busy = true; cx.error = null; render();
        const res = await apiFetch('/api/v1/diagnostics/connect', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ host: cx.host.trim(), port: parseInt(cx.port, 10) || 0, source: cx.source }) });
        cx.busy = false;
        if (res && res.ok) { cx.results.unshift(await res.json()); cx.results = cx.results.slice(0, 5); }
        else cx.error = await errorOf(res, 'The test could not be run.');
        render();
        const box = $('diag-cx-host');
        if (box) { box.focus(); box.select(); }
    }

    function lookupHtml() {
        const tagOf = { answered: ['tag tag-ok', 'Answered'], not_found: ['tag', 'Not found'], timeout: ['tag tag-warn', 'No answer'], error: ['tag tag-warn', 'Error'] };
        const rows = lk.results.map(r => {
            const [cls, label] = tagOf[r.status] || ['tag', r.status];
            const what = r.kind === 'address' ? 'Names of' : 'Addresses of';
            const answers = r.answers.length ? r.answers.map(a => `<code>${escapeHtml(a)}</code>`).join(', ') : `<span class="form-hint">${escapeHtml(r.message)}</span>`;
            return `<div class="fw-row diag-row"><span class="${cls}">${label}</span>
                <div class="fw-main"><div class="fw-name">${what} <strong>${escapeHtml(r.query)}</strong></div><div class="fw-text">${answers}</div></div>
                <span class="form-hint diag-ms">${millisText(r.millis)}</span></div>`;
        }).join('');
        return `<section class="card" id="diag-dns">
            <div class="net-card-head"><h2>DNS lookup</h2></div>
            <p class="form-hint">Asks this router&rsquo;s own DNS server for a name, the way a device on your network would, and shows what it answered and how long it took. Use it to check a local name, a forwarded domain, or whether the upstream servers are answering at all. Type a name such as <code>nas</code> or <code>www.example.com</code>, or an address to see its name. It asks what is running now: changes still in the draft are not included, so commit them first. The servers and names are set on the <a href="#" data-goto="dns">DNS</a> page.</p>
            <div class="diag-ask">
                <input type="text" id="diag-dns-text" value="${escapeHtml(lk.text)}" placeholder="nas, www.example.com or 192.168.1.50" autocomplete="off" spellcheck="false" autocapitalize="off" aria-label="Name or address to look up"${lk.busy ? ' disabled' : ''}>
                <button type="button" class="cfg-btn cfg-btn--primary" id="diag-dns-ask"${lk.busy || !lk.text.trim() ? ' disabled' : ''}>${lk.busy ? 'Asking…' : 'Ask'}</button>
            </div>
            ${lk.error ? `<div class="form-message" role="alert">${escapeHtml(lk.error)}</div>` : ''}
            <div class="fw-list">${rows}</div>
        </section>`;
    }

    function syslogHtml() {
        const rows = (sl.results || []).map(r => `<div class="fw-row diag-row"><span class="tag ${r.ok ? 'tag-ok' : 'tag-warn'}">${r.ok ? 'Sent' : 'Failed'}</span>
            <div class="fw-main"><div class="fw-name"><strong>${escapeHtml(r.host)}</strong><span class="cx-port">:${r.port}</span> ${escapeHtml(r.protocol.toUpperCase())}</div><div class="fw-text form-hint">${escapeHtml(r.message)}</div></div></div>`).join('');
        const none = sl.results && !sl.results.length ? '<p class="form-hint">No syslog server is running yet. Servers set up in Settings start receiving once the change is committed.</p>' : '';
        return `<section class="card" id="diag-syslog">
            <div class="net-card-head"><h2>Syslog test</h2></div>
            <p class="form-hint">Sends one test message to every syslog server that is running now and says what happened to each: a TCP server is reached or it is not; a UDP message is sent and nobody can say whether it arrived, so look on the server. The servers are set up in <a href="#" data-goto="settings">Settings</a>.</p>
            <div class="diag-ask"><button type="button" class="cfg-btn cfg-btn--primary" id="diag-syslog-send"${sl.busy ? ' disabled' : ''}>${sl.busy ? 'Sending…' : 'Send a test message'}</button></div>
            ${sl.error ? `<div class="form-message" role="alert">${escapeHtml(sl.error)}</div>` : ''}${none}
            <div class="fw-list">${rows}</div>
        </section>`;
    }

    function render() {
        $('diagnostics-body').innerHTML = jobCardHtml('ping') + jobCardHtml('traceroute') + connectHtml() + lookupHtml() + syslogHtml();
        ['ping', 'traceroute'].forEach(t => { const o = $('diag-' + t + '-out'); if (o) o.scrollTop = o.scrollHeight; });
    }

    async function sendSyslogTest() {
        if (sl.busy) return;
        sl.busy = true; sl.error = null; render();
        const res = await apiFetch('/api/v1/syslog/test', { method: 'POST' });
        sl.busy = false;
        if (res && res.ok) {
            sl.results = (await res.json()).results || [];
        } else {
            let err = {};
            try { err = res ? await res.json() : {}; } catch (e) { /* not JSON */ }
            sl.error = err.error || (res ? 'The test could not be run.' : 'The router did not answer. Try again.');
        }
        render();
    }

    async function ask() {
        const text = lk.text.trim();
        if (!text || lk.busy) return;
        lk.busy = true; lk.error = null; render();
        const res = await apiFetch('/api/v1/dns/lookup?name=' + encodeURIComponent(text));
        lk.busy = false;
        if (res && res.ok) {
            lk.results.unshift(await res.json());
            lk.results = lk.results.slice(0, 5);
        } else {
            let err = {};
            try { err = res ? await res.json() : {}; } catch (e) { /* not JSON */ }
            lk.error = err.error || (res ? 'The lookup failed.' : 'The router did not answer. Try again.');
        }
        render();
        const box = $('diag-dns-text');
        if (box) { box.focus(); box.select(); }
    }

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('diagnostics-body');
        body.addEventListener('click', (e) => {
            const id = e.target.id;
            if (id === 'diag-dns-ask') ask();
            else if (id === 'diag-syslog-send') sendSyslogTest();
            else if (id === 'diag-ping-start') startJob('ping');
            else if (id === 'diag-traceroute-start') startJob('traceroute');
            else if (id === 'diag-ping-stop') stopJob('ping');
            else if (id === 'diag-traceroute-stop') stopJob('traceroute');
            else if (id === 'diag-cx-go') tryConnect();
        });
        body.addEventListener('input', (e) => {
            const id = e.target.id, v = e.target.value;
            if (id === 'diag-dns-text') { lk.text = v; $('diag-dns-ask').disabled = !lk.text.trim(); }
            else if (id === 'diag-ping-text') { jobs.ping.text = v; $('diag-ping-start').disabled = !v.trim() || !meta.tools.ping; }
            else if (id === 'diag-traceroute-text') { jobs.traceroute.text = v; $('diag-traceroute-start').disabled = !v.trim() || !meta.tools.traceroute; }
            else if (id === 'diag-ping-count') jobs.ping.count = v;
            else if (id === 'diag-cx-host') { cx.host = v; $('diag-cx-go').disabled = !v.trim() || !cx.port; }
            else if (id === 'diag-cx-port') { cx.port = v; $('diag-cx-go').disabled = !cx.host.trim() || !v; }
        });
        body.addEventListener('change', (e) => {
            if (e.target.id === 'diag-ping-source') jobs.ping.source = e.target.value;
            else if (e.target.id === 'diag-cx-source') cx.source = e.target.value;
        });
        body.addEventListener('keydown', (e) => {
            if (e.key !== 'Enter') return;
            const id = e.target.id;
            if (id === 'diag-dns-text') { e.preventDefault(); ask(); }
            else if (id === 'diag-ping-text' || id === 'diag-ping-count') { e.preventDefault(); startJob('ping'); }
            else if (id === 'diag-traceroute-text') { e.preventDefault(); startJob('traceroute'); }
            else if (id === 'diag-cx-host' || id === 'diag-cx-port') { e.preventDefault(); tryConnect(); }
        });
    }

    function init() {
        wire();
        lk = { text: '', busy: false, results: [], error: null };
        sl = { busy: false, results: null, error: null };
        jobs = { ping: { text: '', count: 4, source: 'auto', job: null, busy: false, error: null }, traceroute: { text: '', job: null, busy: false, error: null } };
        cx = { host: '', port: '', source: 'auto', busy: false, results: [], error: null };
        render();
        apiFetch('/api/v1/diagnostics').then(async (res) => {
            if (res && res.ok) { meta = await res.json(); render(); }
        });
        // a test left running by an earlier visit is not followed any more: the page starts afresh
        return () => { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } };
    }

    return { init };
})();
