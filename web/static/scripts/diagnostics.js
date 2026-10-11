// web/static/scripts/diagnostics.js
// Diagnostics page: small tools for finding out why something does not work: the DNS lookup (asks the router's own DNS
// server a question and shows the answer and how long it took) and the syslog test (sends a test message to the syslog
// servers). They change nothing.

window.DiagnosticsModule = (function () {
    let lk = { text: '', busy: false, results: [], error: null };   // what was typed, and the latest answers (newest first)
    let sl = { busy: false, results: null, error: null };   // the syslog test
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const millisText = (ms) => (ms < 10 ? ms.toFixed(1) : ms.toFixed(0)) + ' ms';

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
        $('diagnostics-body').innerHTML = lookupHtml() + syslogHtml();
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
            if (e.target.id === 'diag-dns-ask') ask();
            else if (e.target.id === 'diag-syslog-send') sendSyslogTest();
        });
        body.addEventListener('input', (e) => {
            if (e.target.id !== 'diag-dns-text') return;
            lk.text = e.target.value;
            $('diag-dns-ask').disabled = !lk.text.trim();
        });
        body.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && e.target.id === 'diag-dns-text') { e.preventDefault(); ask(); }
        });
    }

    function init() {
        wire();
        lk = { text: '', busy: false, results: [], error: null };
        sl = { busy: false, results: null, error: null };
        render();
        return () => {};
    }

    return { init };
})();
