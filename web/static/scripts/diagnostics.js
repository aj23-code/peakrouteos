// web/static/scripts/diagnostics.js
// Diagnostics page: small tools for finding out why something does not work. Today one: the DNS lookup, which asks the
// router's own DNS server a question and shows the answer and how long it took. It changes nothing.

window.DiagnosticsModule = (function () {
    let lk = { text: '', busy: false, results: [], error: null };   // what was typed, and the latest answers (newest first)
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

    function render() {
        $('diagnostics-body').innerHTML = lookupHtml();
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
        render();
        return () => {};
    }

    return { init };
})();
