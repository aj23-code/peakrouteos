// web/static/scripts/routes.js
// Routes page: the static routes (edited as a draft) and the routing table the router is using now.

window.RoutesModule = (function () {
    const KIND_TEXT = { default: 'Default', connected: 'Connected', static: 'Static', via: 'Other' };
    const KIND_TIP = {
        default: 'Where everything else goes',
        connected: 'A network this router is directly on',
        static: 'One of the routes on this page',
        via: 'Reached through a gateway; set up by something other than this page',
    };

    let server = [];          // the saved draft, as the API sent it
    let rows = [];            // what the form shows: { name, destination, gateway, metric, enabled }
    let errors = {};          // "routes[2].gateway" -> message, as the API reports them
    let editable = true;
    let saving = false;
    let wired = false;
    let tableTimer = null;

    const $ = (id) => document.getElementById(id);
    const blank = () => ({ name: '', destination: '', gateway: '', metric: '', enabled: true });
    const fromServer = (r) => ({ name: r.name || '', destination: r.destination || '', gateway: r.gateway || '', metric: r.metric ? String(r.metric) : '', enabled: r.enabled !== false });
    const isBlankRow = (r) => !r.name.trim() && !r.destination.trim() && !r.gateway.trim() && !r.metric.trim();

    function showMessage(text, kind) {
        const box = $('routes-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    // ---------------------------------------------------------------- the form

    function fieldHtml(i, field, label, value, attrs) {
        const err = errors[`routes[${i}].${field}`];
        const id = `route-${i}-${field}`;
        return `<div class="form-field">
            <label for="${id}">${label}</label>
            <input type="text" id="${id}" data-row="${i}" data-field="${field}" value="${escapeHtml(value)}" autocomplete="off" spellcheck="false"${attrs || ''}${err ? ' aria-invalid="true"' : ''}${editable && !saving ? '' : ' disabled'}>
            ${err ? `<p class="field-error">${escapeHtml(err)}</p>` : ''}
        </div>`;
    }

    function render() {
        const list = $('routes-list');
        list.innerHTML = rows.map((r, i) => `
            <div class="route-row${r.enabled ? '' : ' route-row--off'}" data-row="${i}">
                <div class="route-fields">
                    ${fieldHtml(i, 'name', 'Name', r.name, ' placeholder="Optional" maxlength="40"')}
                    ${fieldHtml(i, 'destination', 'Network', r.destination, ' placeholder="10.20.0.0/16" inputmode="decimal"')}
                    ${fieldHtml(i, 'gateway', 'Through', r.gateway, ' placeholder="192.168.1.50" inputmode="decimal"')}
                    ${fieldHtml(i, 'metric', 'Metric', r.metric, ' placeholder="Default" inputmode="numeric"')}
                </div>
                <div class="route-controls">
                    <label class="toggle"><input type="checkbox" data-row="${i}" data-field="enabled"${r.enabled ? ' checked' : ''}${editable && !saving ? '' : ' disabled'}> On</label>
                    <button type="button" class="cfg-btn cfg-btn--ghost route-remove" data-row="${i}" aria-label="Remove route ${i + 1}"${editable && !saving ? '' : ' disabled'}>Remove</button>
                </div>
            </div>`).join('');
        $('routes-empty').classList.toggle('hidden', rows.length > 0);
        updateState();
    }

    function normalised(list) {
        return JSON.stringify(list.filter(r => !isBlankRow(r)).map(r => [r.name.trim(), r.destination.trim(), r.gateway.trim(), r.metric.trim(), r.enabled]));
    }

    function isDirty() {
        return normalised(rows) !== normalised(server.map(fromServer));
    }

    function updateState() {
        $('routes-draft').classList.toggle('hidden', !pending);
        const dirty = isDirty();
        const lock = !editable || saving;
        $('routes-hint').textContent = !editable
            ? 'A change is being applied or waits for confirmation: routes cannot be edited right now.'
            : (dirty ? 'You have unsaved changes.' : '');
        $('routes-add').disabled = lock || rows.length >= 64;
        $('routes-save').disabled = lock || !dirty;
        $('routes-reset').disabled = saving || !dirty;
    }

    let pending = false;

    // ---------------------------------------------------------------- the routing table

    function renderTable(table) {
        const body = $('routes-table');
        if (!table || !table.length) {
            body.innerHTML = '<tr><td colspan="5" class="table-placeholder">No routes reported.</td></tr>';
            return;
        }
        body.innerHTML = table.map(r => `<tr>
            <td data-label="Network"><strong>${escapeHtml(r.kind === 'default' ? 'Everything else' : r.destination)}</strong></td>
            <td data-label="Through">${r.gateway ? escapeHtml(r.gateway) : '<span class="form-hint">directly connected</span>'}</td>
            <td data-label="Port"><span class="chip">${escapeHtml(r.interface)}</span></td>
            <td data-label="Metric">${r.metric}</td>
            <td data-label="Type"><span class="tag ${r.kind === 'static' ? 'tag-ok' : ''}" title="${escapeHtml(KIND_TIP[r.kind] || '')}">${escapeHtml(KIND_TEXT[r.kind] || r.kind)}</span></td>
        </tr>`).join('');
    }

    // ---------------------------------------------------------------- loading and saving

    async function load(keepEdits) {
        const res = await apiFetch('/api/v1/routes');
        if (!res || !res.ok) { showMessage('Could not load the routes.', 'error'); return; }
        const data = await res.json();
        editable = data.editable !== false;
        pending = !!data.pending;
        server = data.routes || [];
        renderTable(data.table);
        if (!keepEdits) {
            rows = server.map(fromServer);
            errors = {};
            render();
        } else {
            updateState();
        }
    }

    async function save() {
        showMessage('');
        errors = {};

        // Rows left completely empty are ignored; remember where the others were so that an error
        // reported for "routes[1]" lands on the row it belongs to
        const sent = [];
        const origin = [];
        let bad = false;
        rows.forEach((r, i) => {
            if (isBlankRow(r)) return;
            const metric = r.metric.trim();
            if (metric && !/^\d{1,5}$/.test(metric)) {
                errors[`routes[${i}].metric`] = 'must be a number';
                bad = true;
            }
            sent.push({ name: r.name.trim(), destination: r.destination.trim(), gateway: r.gateway.trim(), metric: metric ? parseInt(metric, 10) : 0, enabled: r.enabled });
            origin.push(i);
        });
        if (bad) { showMessage('Some routes need attention.', 'error'); render(); return; }

        saving = true;
        render();
        const res = await apiFetch('/api/v1/routes', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ routes: sent }),
        });
        saving = false;

        if (!res) { showMessage('The router did not answer. Try again.', 'error'); render(); return; }
        if (!res.ok) {
            let err = {};
            try { err = await res.json(); } catch (e) { /* not JSON */ }
            const general = [];
            (err.errors || []).forEach(e => {
                const m = /^routes\[(\d+)\]\.(\w+)$/.exec(e.field);
                if (m && origin[+m[1]] !== undefined) {
                    const key = `routes[${origin[+m[1]]}].${m[2]}`;
                    errors[key] = errors[key] ? errors[key] + ' ' + e.message : e.message;
                } else {
                    general.push(e.message);
                }
            });
            showMessage(general.join(' ') || (Object.keys(errors).length ? 'Some routes need attention.' : (err.error || `Saving failed (${res.status}).`)), 'error');
            render();
            return;
        }

        showMessage('Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        await load(false);
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    function wire() {
        if (wired) return;
        wired = true;

        const list = $('routes-list');
        list.addEventListener('input', (e) => {
            const el = e.target;
            if (!el.dataset || el.dataset.row === undefined || el.type === 'checkbox') return;
            const i = +el.dataset.row;
            rows[i][el.dataset.field] = el.value;
            // an edited field is no longer wrong until it is saved again
            delete errors[`routes[${i}].${el.dataset.field}`];
            el.removeAttribute('aria-invalid');
            const msg = el.parentElement.querySelector('.field-error');
            if (msg) msg.remove();
            updateState();
        });
        list.addEventListener('change', (e) => {
            const el = e.target;
            if (el.type !== 'checkbox' || el.dataset.row === undefined) return;
            rows[+el.dataset.row].enabled = el.checked;
            el.closest('.route-row').classList.toggle('route-row--off', !el.checked);
            updateState();
        });
        list.addEventListener('click', (e) => {
            const btn = e.target.closest('.route-remove');
            if (!btn) return;
            rows.splice(+btn.dataset.row, 1);
            errors = {}; // the row numbers shifted
            render();
        });

        $('routes-add').addEventListener('click', () => {
            rows.push(blank());
            render();
            const last = $(`route-${rows.length - 1}-destination`);
            if (last) last.focus();
        });
        $('routes-save').addEventListener('click', save);
        $('routes-reset').addEventListener('click', () => {
            rows = server.map(fromServer);
            errors = {};
            showMessage('');
            render();
        });
        list.addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && e.target.tagName === 'INPUT' && e.target.type === 'text' && !$('routes-save').disabled) {
                e.preventDefault();
                save();
            }
        });
    }

    function init() {
        wire();
        showMessage('');
        rows = [];
        errors = {};
        server = [];
        render();
        load(false);
        // the table changes when a commit lands; never replace what the person is typing
        tableTimer = setInterval(() => load(true), 10000);
        return () => { clearInterval(tableTimer); tableTimer = null; };
    }

    return { init };
})();
