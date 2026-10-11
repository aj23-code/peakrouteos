// web/static/scripts/firewall_rules.js
// Firewall rules page: one list of rules per place traffic comes from (a Local Network, or the internet), checked
// top to bottom. The fixed lines the router always applies are shown above and below the rules so that "why does
// this not work?" has a visible answer. Saving only writes a draft. docs/firewall.md explains the model.

window.FirewallRulesModule = (function () {
    let data = null;            // GET /api/v1/firewall
    let tab = 'lan';            // the source on show
    let editing = null;         // { id: null (new) or a rule id, form, errors, message }
    let counters = {};          // rule id -> { packets, bytes } of the rules in effect
    let countersKnown = false;
    let confirmDelete = null;   // the id of the rule whose delete button is armed
    let busy = false;
    let wired = false;
    let timer = null;

    const $ = (id) => document.getElementById(id);
    const editable = () => !data || data.editable !== false;

    const ACTIONS = [['allow', 'Allow'], ['block', 'Block'], ['reject', 'Reject']];
    const PROTOCOLS = [['any', 'Any protocol'], ['tcp', 'TCP'], ['udp', 'UDP'], ['tcp+udp', 'TCP and UDP'], ['icmp', 'ICMP (ping)']];
    const hasPorts = (p) => p === 'tcp' || p === 'udp' || p === 'tcp+udp';

    function showMessage(text, kind) {
        const box = $('fw-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    const tabInfo = (id) => (data.tabs || []).find(t => t.id === id);
    const rulesOf = (id) => (data.rules || []).filter(r => r.from === id);
    const option = (value, label, current) => `<option value="${escapeHtml(value)}"${String(value) === String(current) ? ' selected' : ''}>${escapeHtml(label)}</option>`;

    function nameOf(id) {
        if (id === 'any') return 'Anywhere';
        if (id === 'router') return 'This router';
        if (id === 'internet' || id === 'wan') return 'The internet';
        const t = tabInfo(id);
        return t ? t.name : id;
    }

    // ---------------------------------------------------------------- the list

    function lineHtml(l) {
        const cls = l.kind === 'block' ? 'fw-line--block' : (l.kind === 'off' ? 'fw-line--off' : '');
        const icon = l.kind === 'block' ? '✕' : (l.kind === 'off' ? '–' : '✓');
        return `<div class="fw-row fw-line ${cls}"><span class="fw-lock" aria-hidden="true">\u{1F512}</span>
            <span class="fw-line-mark" aria-hidden="true">${icon}</span><span class="fw-line-text">${escapeHtml(l.text)}</span></div>`;
    }

    function actionTag(r) {
        const cls = r.action === 'allow' ? 'tag-ok' : (r.action === 'block' ? 'tag-warn' : 'tag-draft');
        const label = (ACTIONS.find(a => a[0] === r.action) || [0, r.action])[1];
        return `<span class="tag ${cls}">${escapeHtml(label)}</span>`;
    }

    function hitsText(r) {
        if (r.pending) return '–';
        const c = counters[r.id];
        if (!countersKnown || !c) return '–';
        return c.packets.toLocaleString();
    }

    function ruleHtml(r, i, all) {
        const lock = !editable() || busy;
        const shadow = r.shadowed_by
            ? `<div class="fw-warn">Never matches: the rule <strong>${escapeHtml((data.rules.find(x => x.id === r.shadowed_by) || {}).name || r.shadowed_by)}</strong> above already decides all of this traffic.</div>` : '';
        const armed = confirmDelete === r.id;
        return `<div class="fw-row fw-rule${r.enabled ? '' : ' fw-rule--off'}" data-id="${escapeHtml(r.id)}">
            <div class="fw-move">
                <button type="button" class="fw-icon-btn fw-up" data-id="${escapeHtml(r.id)}" aria-label="Move up"${lock || i === 0 ? ' disabled' : ''}>▲</button>
                <button type="button" class="fw-icon-btn fw-down" data-id="${escapeHtml(r.id)}" aria-label="Move down"${lock || i === all.length - 1 ? ' disabled' : ''}>▼</button>
            </div>
            <label class="toggle fw-switch" title="${r.enabled ? 'Switched on' : 'Switched off'}"><input type="checkbox" class="fw-enable" data-id="${escapeHtml(r.id)}"${r.enabled ? ' checked' : ''}${lock ? ' disabled' : ''} aria-label="Switch ${escapeHtml(r.name)} on or off"></label>
            <div class="fw-main">
                <div class="fw-name"><strong>${escapeHtml(r.name)}</strong> ${actionTag(r)}${r.pending ? ' <span class="tag tag-draft">Draft</span>' : ''}${r.log ? ' <span class="tag">Logged</span>' : ''}</div>
                <div class="form-hint fw-text">${escapeHtml(r.text)}</div>
                ${shadow}
            </div>
            <div class="fw-hits" title="Packets that matched since the rules were last loaded"><span class="form-hint">Hits</span><span class="fw-hits-n" data-id="${escapeHtml(r.id)}">${hitsText(r)}</span></div>
            <div class="fw-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost fw-edit" data-id="${escapeHtml(r.id)}"${lock ? ' disabled' : ''}>Edit</button>
                <button type="button" class="cfg-btn cfg-btn--danger fw-delete" data-id="${escapeHtml(r.id)}"${lock ? ' disabled' : ''}>${armed ? 'Click again' : 'Delete'}</button>
            </div>
        </div>`;
    }

    // ---------------------------------------------------------------- the editor

    function blankForm() {
        const toOptions = destinations();
        return { name: '', action: 'allow', protocol: 'any', source: '', to: toOptions.length ? toOptions[0][0] : 'any', destination: '', ports: '', log: false, enabled: true };
    }

    function formOf(r) {
        return { name: r.name, action: r.action, protocol: r.protocol, source: r.source, to: r.to, destination: r.destination, ports: r.ports, log: r.log, enabled: r.enabled };
    }

    // where a rule on this tab may send traffic: anywhere, the router, the internet (not from the internet), other networks
    function destinations() {
        const out = [['internet', 'The internet'], ['any', 'Anywhere (this router and everything behind it)'], ['router', 'This router']];
        const list = tab === 'wan' ? out.filter(o => o[0] !== 'internet') : out;
        (data.tabs || []).filter(t => t.kind === 'network' && t.id !== tab).forEach(t => list.push([t.id, t.name]));
        return list;
    }

    function fieldHtml(label, id, control, key, hint) {
        const err = editing.errors[key];
        return `<div class="form-field"><label for="${id}">${label}</label>${control}${hint ? `<p class="form-hint">${hint}</p>` : ''}${err ? `<p class="field-error">${escapeHtml(err)}</p>` : ''}</div>`;
    }

    function editorHtml() {
        const f = editing.form;
        const lock = !editable() || busy;
        const dis = lock ? ' disabled' : '';
        const text = (id, key, ph, mode) => `<input type="text" id="${id}" data-fw="${key}" value="${escapeHtml(f[key])}" placeholder="${escapeHtml(ph)}" autocomplete="off" spellcheck="false" autocapitalize="off"${mode ? ` inputmode="${mode}"` : ''}${editing.errors[key] ? ' aria-invalid="true"' : ''}${dis}>`;
        const select = (id, key, opts) => `<select id="${id}" class="role-select" data-fw="${key}"${dis}>${opts.map(o => option(o[0], o[1], f[key])).join('')}</select>`;
        const t = tabInfo(tab);
        return `<section class="card fw-editor" id="fw-editor">
            <div class="net-card-head"><h2>${editing.id ? 'Edit rule' : 'New rule'}</h2><span class="form-hint">for traffic coming from ${escapeHtml(t.name)}</span></div>
            <div class="form-grid">
                ${fieldHtml('Name', 'fw-f-name', text('fw-f-name', 'name', 'Printer from the LAN'), 'name')}
                ${fieldHtml('What to do', 'fw-f-action', select('fw-f-action', 'action', ACTIONS), 'action',
                    f.action === 'reject' ? 'Reject tells the sender at once that it was refused; Block says nothing.' : '')}
                ${fieldHtml('Going to', 'fw-f-to', select('fw-f-to', 'to', destinations()), 'to')}
                ${fieldHtml('Protocol', 'fw-f-protocol', select('fw-f-protocol', 'protocol', PROTOCOLS), 'protocol')}
                ${hasPorts(f.protocol) ? fieldHtml('Ports', 'fw-f-ports', text('fw-f-ports', 'ports', '80, 443, 8000-8100', 'numeric'), 'ports', 'The ports it is going to. Empty means all of them.') : ''}
                ${fieldHtml('From address <span class="form-hint">(optional)</span>', 'fw-f-source', text('fw-f-source', 'source', t.kind === 'network' ? 'one device, or a range like 192.168.1.0/28' : 'any'), 'source')}
                ${fieldHtml('To address <span class="form-hint">(optional)</span>', 'fw-f-destination', text('fw-f-destination', 'destination', 'one device, or a range'), 'destination')}
            </div>
            <div class="net-access">
                <label class="toggle toggle--block"><input type="checkbox" data-fw="log"${f.log ? ' checked' : ''}${dis}><span><strong>Log matching traffic</strong><span class="form-hint">Writes a line to the system log for what this rule matches (a few per second at most).</span></span></label>
                <label class="toggle toggle--block"><input type="checkbox" data-fw="enabled"${f.enabled ? ' checked' : ''}${dis}><span><strong>Switched on</strong><span class="form-hint">Off keeps the rule here without applying it.</span></span></label>
            </div>
            ${editing.message ? `<div class="form-message" role="alert">${escapeHtml(editing.message)}</div>` : ''}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" id="fw-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="fw-save"${lock ? ' disabled' : ''}>${editing.id ? 'Save to draft' : 'Add rule'}</button>
            </div>
        </section>`;
    }

    // ---------------------------------------------------------------- rendering

    function render() {
        if (!data) return;
        if (!tabInfo(tab)) tab = 'lan';
        const sel = $('fw-tab');
        sel.innerHTML = (data.tabs || []).map(t => option(t.id, `${t.name} (${rulesOf(t.id).length})`, tab)).join('');
        sel.disabled = !!editing && busy;
        $('fw-add').disabled = !editable() || busy || !!editing;

        const t = tabInfo(tab);
        const policy = (data.policy || {})[tab] || { before: [], after: [] };
        const rules = rulesOf(tab);
        const switches = t.kind === 'network'
            ? `Whether this network may reach the internet or the other networks is set on <a href="#" data-goto="networks">Local Networks</a>; the lines in grey below show what those switches mean.`
            : 'Nothing from the internet gets in unless a rule or a port forward lets it. Replies to connections you started are always allowed.';
        $('fw-body').innerHTML = `<section class="card fw-card">
            <div class="net-card-head"><h2>Traffic from ${escapeHtml(t.name)}</h2><span class="tag">${rules.length} ${rules.length === 1 ? 'rule' : 'rules'}</span></div>
            <p class="form-hint">${switches}</p>
            ${!editable() ? '<p class="form-hint">A change is being applied or waits for confirmation: rules cannot be edited right now.</p>' : ''}
            <div class="fw-list">
                ${policy.before.map(lineHtml).join('')}
                ${rules.length ? rules.map(ruleHtml).join('') : '<div class="fw-row fw-empty">No rules of your own yet. Add one to let something through or to keep something out.</div>'}
                ${policy.after.map(lineHtml).join('')}
            </div>
        </section>${editing ? editorHtml() : ''}`;
    }

    function updateHits() {
        document.querySelectorAll('.fw-hits-n').forEach(el => {
            const r = (data.rules || []).find(x => x.id === el.dataset.id);
            if (!r) return;
            el.textContent = hitsText(r);
            const c = counters[r.id];
            el.parentElement.title = c && countersKnown && !r.pending
                ? `${c.packets.toLocaleString()} packets, ${formatBytes(c.bytes)} since the rules were last loaded`
                : 'Packets that matched since the rules were last loaded';
        });
    }

    // ---------------------------------------------------------------- loading and saving

    async function load() {
        const res = await apiFetch('/api/v1/firewall');
        if (!res || !res.ok) { showMessage('Could not load the firewall rules.', 'error'); return; }
        data = await res.json();
        render();
    }

    async function loadCounters() {
        const res = await apiFetch('/api/v1/firewall/counters');
        if (!res || !res.ok) return;
        const body = await res.json();
        countersKnown = !!body.known;
        counters = body.counters || {};
        if (data) updateHits();
    }

    async function errorOf(res, fallback) {
        if (!res) return { general: 'The router did not answer. Try again.', fields: [] };
        let err = {};
        try { err = await res.json(); } catch (e) { /* not JSON */ }
        return { general: err.error || fallback, fields: err.errors || [] };
    }

    function afterChange(message) {
        showMessage(message, 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    async function save() {
        const f = editing.form;
        const body = { name: f.name, action: f.action, to: f.to, protocol: f.protocol, source: f.source, destination: f.destination, log: f.log, enabled: f.enabled,
            ports: hasPorts(f.protocol) ? f.ports : '' };
        if (!editing.id) body.from = tab;
        editing.errors = {};
        editing.message = null;
        busy = true;
        render();
        const res = await apiFetch(editing.id ? '/api/v1/firewall/rules/' + encodeURIComponent(editing.id) : '/api/v1/firewall/rules', {
            method: editing.id ? 'PUT' : 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
        });
        busy = false;
        if (!res || !res.ok) {
            const e = await errorOf(res, `Saving failed${res ? ' (' + res.status + ')' : ''}.`);
            const general = [];
            e.fields.forEach(x => {
                if (['name', 'action', 'to', 'protocol', 'source', 'destination', 'ports'].includes(x.field)) editing.errors[x.field] = x.message;
                else general.push(x.message);
            });
            editing.message = general.join(' ') || (Object.keys(editing.errors).length ? 'Some settings need attention.' : e.general);
            render();
            return;
        }
        const wasNew = !editing.id;
        editing = null;
        await load();
        afterChange(wasNew ? 'Rule added to the draft. Review and commit it from the bar at the top when you are ready.' : 'Saved as a draft. Review and commit it from the bar at the top when you are ready.');
    }

    async function toggle(id, on) {
        busy = true; render();
        const res = await apiFetch('/api/v1/firewall/rules/' + encodeURIComponent(id), { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ enabled: on }) });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'The rule could not be changed.'); showMessage(e.fields.map(x => x.message).join(' ') || e.general, 'error'); await load(); return; }
        await load();
        afterChange(on ? 'Rule switched on in the draft.' : 'Rule switched off in the draft.');
    }

    async function remove(id) {
        if (confirmDelete !== id) {
            confirmDelete = id;
            render();
            setTimeout(() => { if (confirmDelete === id) { confirmDelete = null; render(); } }, 4000);
            return;
        }
        confirmDelete = null;
        busy = true; render();
        const res = await apiFetch('/api/v1/firewall/rules/' + encodeURIComponent(id), { method: 'DELETE' });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'The rule could not be deleted.'); showMessage(e.general, 'error'); await load(); return; }
        await load();
        afterChange('Rule removed from the draft. Review and commit it from the bar at the top when you are ready.');
    }

    async function move(id, delta) {
        const ids = rulesOf(tab).map(r => r.id);
        const i = ids.indexOf(id), j = i + delta;
        if (i < 0 || j < 0 || j >= ids.length) return;
        [ids[i], ids[j]] = [ids[j], ids[i]];
        busy = true; render();
        const res = await apiFetch('/api/v1/firewall/order', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ from: tab, ids }) });
        busy = false;
        if (!res || !res.ok) { const e = await errorOf(res, 'The order could not be changed.'); showMessage(e.fields.map(x => x.message).join(' ') || e.general, 'error'); await load(); return; }
        await load();
        afterChange('New order saved in the draft. Review and commit it from the bar at the top when you are ready.');
    }

    // ---------------------------------------------------------------- events

    function wire() {
        if (wired) return;
        wired = true;
        $('fw-tab').addEventListener('change', (e) => { tab = e.target.value; editing = null; confirmDelete = null; showMessage(''); render(); });
        $('fw-add').addEventListener('click', () => {
            editing = { id: null, form: blankForm(), errors: {}, message: null };
            render();
            const f = $('fw-f-name'); if (f) f.focus();
        });
        const body = $('fw-body');
        body.addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            const id = btn.dataset.id;
            if (btn.classList.contains('fw-up')) move(id, -1);
            else if (btn.classList.contains('fw-down')) move(id, 1);
            else if (btn.classList.contains('fw-delete')) remove(id);
            else if (btn.classList.contains('fw-edit')) {
                const r = (data.rules || []).find(x => x.id === id);
                if (r) { editing = { id, form: formOf(r), errors: {}, message: null }; render(); const f = $('fw-f-name'); if (f) f.focus(); }
            } else if (btn.id === 'fw-save') save();
            else if (btn.id === 'fw-cancel') { editing = null; render(); }
        });
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (el.classList.contains('fw-enable')) { toggle(el.dataset.id, el.checked); return; }
            const key = el.dataset && el.dataset.fw;
            if (!key || !editing) return;
            editing.form[key] = el.type === 'checkbox' ? el.checked : el.value;
            delete editing.errors[key];
            if (key === 'protocol' || key === 'action') render(); // the ports field and a hint depend on these
        });
        body.addEventListener('input', (e) => {
            const el = e.target;
            const key = el.dataset && el.dataset.fw;
            if (!key || !editing || el.type === 'checkbox' || el.tagName === 'SELECT') return;
            editing.form[key] = el.value;
            delete editing.errors[key];
            el.removeAttribute('aria-invalid');
            const msg = el.closest('.form-field').querySelector('.field-error');
            if (msg) msg.remove();
        });
        body.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && editing && e.target.tagName === 'INPUT' && e.target.type === 'text' && editable() && !busy) { e.preventDefault(); save(); }
        });
    }

    function init() {
        wire();
        showMessage('');
        data = null;
        editing = null;
        confirmDelete = null;
        counters = {};
        countersKnown = false;
        load();
        loadCounters();
        timer = setInterval(loadCounters, 5000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
