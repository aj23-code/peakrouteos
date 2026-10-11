// web/static/scripts/firewall_log.js
// Firewall, Logs page: the packets that rules with "Log matching traffic" on matched, newest first, in words, with filters
// and a CSV download. The router keeps them itself (see docs/firewall.md, section 7). Read only.

window.FirewallLogModule = (function () {
    const PAGE = 200;
    let res = null;             // the latest GET /api/v1/firewall/log (the first page)
    let rows = [];              // the entries shown: the first page and any "show more" pages after it
    let next = 0;               // the cursor for the next page (0: that was all)
    let fw = null;              // GET /api/v1/firewall: the rules and the sources, for the filters
    let names = new Map();      // IP address -> device name, from the DHCP leases
    let timer = null;
    let paused = false;
    let loading = false;
    let wired = false;

    const $ = (id) => document.getElementById(id);

    function hostName(ip) {
        const n = names.get(ip);
        return n ? `<strong>${escapeHtml(n)}</strong> <span class="form-hint">${escapeHtml(ip)}</span>` : escapeHtml(ip);
    }

    const endpoint = (ip, port) => `${hostName(ip)}${port ? `<span class="cx-port">:${port}</span>` : ''}`;

    function when(ms) {
        const d = new Date(ms);
        const today = new Date();
        const time = d.toLocaleTimeString();
        return d.toDateString() === today.toDateString() ? time : `${d.toLocaleDateString(undefined, { month: 'short', day: 'numeric' })} ${time}`;
    }

    function actionTag(e) {
        if (!e.action) return '';
        const cls = e.action === 'allow' ? 'tag-ok' : (e.action === 'block' ? 'tag-warn' : 'tag-draft');
        return ` <span class="tag ${cls}">${escapeHtml(e.action)}</span>`;
    }

    function rowHtml(e) {
        const path = `${e.in || '?'} → ${e.out || 'this router'}`;
        const until = e.count > 1 ? `<div class="form-hint">until ${escapeHtml(when(e.last))}</div>` : '';
        const rule = `<strong>${escapeHtml(e.name)}</strong>` + (e.action ? actionTag(e) : ' <span class="form-hint">(rule no longer there)</span>');
        return `<tr title="${escapeHtml(path)}"><td>${escapeHtml(when(e.time))}${until}</td>
            <td>${rule}</td>
            <td>${endpoint(e.src, e.sport)}</td><td>${endpoint(e.dst, e.dport)}</td><td>${escapeHtml(e.proto)}</td>
            <td>${e.count > 1 ? `×${e.count.toLocaleString()}` : '1'}</td></tr>`;
    }

    // the query string of the filters (without paging)
    function filters() {
        const p = new URLSearchParams();
        const set = (k, id) => { const v = $(id).value.trim(); if (v) p.set(k, v); };
        set('rule', 'fwl-rule'); set('action', 'fwl-action'); set('network', 'fwl-network'); set('hours', 'fwl-hours'); set('q', 'fwl-q');
        return p;
    }

    function fillFilters() {
        if (!fw) return;
        const keep = (id) => $(id).value;
        const fill = (id, first, options) => {
            const sel = $(id), was = keep(id);
            sel.innerHTML = `<option value="">${first}</option>` + options.map(([v, label]) => `<option value="${escapeHtml(v)}">${escapeHtml(label)}</option>`).join('');
            sel.value = options.some(o => o[0] === was) ? was : '';
        };
        fill('fwl-rule', 'All rules', (fw.rules || []).map(r => [r.id, r.log ? r.name : `${r.name} (not logging)`]));
        fill('fwl-network', 'Anywhere', (fw.tabs || []).map(t => [t.id, t.kind === 'internet' ? 'The internet' : t.name]));
    }

    function render() {
        const body = $('fwl-body'), summary = $('fwl-summary'), more = $('fwl-more');
        if (!res) { body.innerHTML = '<tr><td colspan="6" class="table-placeholder">Reading the firewall log&hellip;</td></tr>'; return; }
        if (!res.known) {
            body.innerHTML = '<tr><td colspan="6" class="table-placeholder">The firewall log cannot be read on this system.</td></tr>';
            summary.textContent = '';
            more.classList.add('hidden');
            return;
        }
        const filtered = [...filters().keys()].length > 0;
        if (!rows.length) {
            body.innerHTML = `<tr><td colspan="6" class="table-placeholder">${filtered || res.total
                ? 'Nothing in the log matches these filters.'
                : 'Nothing has been logged yet. Switch on <em>Log matching traffic</em> for a rule on the <a href="#" data-goto="firewall-rules">Rules</a> page: the packets it matches show up here once the rule is running.'}</td></tr>`;
        } else {
            body.innerHTML = rows.map(rowHtml).join('');
        }
        const since = res.oldest ? `, the oldest from ${when(res.oldest)}` : '';
        summary.textContent = `${res.total.toLocaleString()} ${res.total === 1 ? 'entry' : 'entries'} kept${since}. The log keeps ${res.keep_days} days or ${res.keep_entries.toLocaleString()} entries, whichever comes first; the same kind of packet within a minute is one entry.`;
        more.classList.toggle('hidden', !next);
        $('fwl-csv').href = '/api/v1/firewall/log.csv' + (filtered ? '?' + filters() : '');
    }

    async function load(force) {
        if ((paused && !force) || loading) return;
        loading = true;
        const p = filters();
        p.set('limit', PAGE);
        const [r, f, leases] = await Promise.all([apiFetch('/api/v1/firewall/log?' + p), fw ? null : apiFetch('/api/v1/firewall'), apiFetch('/api/v1/dhcp/leases')]);
        loading = false;
        if (!r || !r.ok) {
            if (r && r.status === 400) { const e = await r.json().catch(() => ({})); $('fwl-summary').textContent = e.error || ''; }
            return;
        }
        if (f && f.ok) { fw = await f.json(); fillFilters(); }
        if (leases && leases.ok) {
            names = new Map();
            ((await leases.json()) || []).forEach(l => { if (l.hostname && l.hostname !== 'Unknown Device') names.set(l.ip_address, l.hostname); });
        }
        const fresh = await r.json();
        // refreshing replaces the first page; pages the person asked for are kept as they are until they refresh themselves
        if (force || rows.length <= PAGE) { res = fresh; rows = fresh.entries; next = fresh.next; } else { res = fresh; }
        render();
    }

    async function more() {
        const p = filters();
        p.set('limit', PAGE);
        p.set('before', next);
        const r = await apiFetch('/api/v1/firewall/log?' + p);
        if (!r || !r.ok) return;
        const page = await r.json();
        rows = rows.concat(page.entries);
        next = page.next;
        render();
    }

    function wire() {
        if (wired) return;
        wired = true;
        const refilter = () => { rows = []; next = 0; load(true); };
        ['fwl-rule', 'fwl-action', 'fwl-network', 'fwl-hours'].forEach(id => $(id).addEventListener('change', refilter));
        let typing = null;
        $('fwl-q').addEventListener('input', () => { clearTimeout(typing); typing = setTimeout(refilter, 300); });
        $('fwl-more').addEventListener('click', more);
        $('fwl-pause').addEventListener('click', () => {
            paused = !paused;
            $('fwl-pause').textContent = paused ? 'Resume' : 'Pause';
            $('fwl-pause').setAttribute('aria-pressed', String(paused));
            if (!paused) load(true);
        });
    }

    function init() {
        wire();
        res = null; rows = []; next = 0; fw = null; paused = false; loading = false;
        $('fwl-pause').textContent = 'Pause';
        $('fwl-pause').setAttribute('aria-pressed', 'false');
        ['fwl-rule', 'fwl-action', 'fwl-network', 'fwl-q'].forEach(id => { $(id).value = ''; });
        $('fwl-hours').value = '';
        render();
        load(true);
        timer = setInterval(() => load(false), 10000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
