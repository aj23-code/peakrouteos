// web/static/scripts/ports.js
// Ports page: pick the role of every network port (Internet/WAN, LAN or unassigned).

window.PortsModule = (function () {
    const ROLE_LABELS = { wan: 'Internet (WAN)', lan: 'LAN', unassigned: 'Unassigned' };

    let pollInterval = null;
    let ports = [];          // last answer from the server
    let editable = true;
    let chosen = {};         // port name -> role currently selected in the table
    let dirty = false;       // the person changed something that is not saved yet
    let saving = false;
    let wired = false;

    const $ = (id) => document.getElementById(id);

    function showMessage(text, kind) {
        const box = $('ports-message');
        if (!box) return;
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    function serverRoles() {
        const roles = {};
        ports.forEach(p => { roles[p.name] = p.role; });
        return roles;
    }

    function isDirty() {
        const saved = serverRoles();
        return ports.some(p => chosen[p.name] !== saved[p.name]);
    }

    // The same rules the server enforces, so the person hears it before pressing Save
    function problem() {
        const roles = ports.map(p => chosen[p.name]);
        const wan = roles.filter(r => r === 'wan').length;
        const lan = roles.filter(r => r === 'lan').length;
        if (wan !== 1) return wan === 0 ? 'Pick one port as Internet (WAN).' : 'Only one port can be Internet (WAN).';
        if (lan < 1) return 'At least one port must be LAN.';
        return '';
    }

    function render() {
        const tbody = $('ports-list');
        if (!tbody) return;

        if (ports.length === 0) {
            tbody.innerHTML = '<tr><td colspan="6" class="table-placeholder">No network ports found.</td></tr>';
        } else {
            tbody.innerHTML = ports.map(p => {
                const role = chosen[p.name];
                const options = Object.keys(ROLE_LABELS).map(r =>
                    `<option value="${r}"${r === role ? ' selected' : ''}>${ROLE_LABELS[r]}</option>`).join('');

                let note = '';
                if (!p.present) {
                    note = '<span class="tag tag-warn">Hardware missing</span>';
                } else if (p.pending && role === p.role) {
                    note = `<span class="tag tag-draft" title="In effect now: ${escapeHtml(ROLE_LABELS[p.active_role] || p.active_role)}">Draft</span>`;
                }
                if (role !== p.role) {
                    note = `<span class="tag tag-draft">Unsaved (was ${escapeHtml(ROLE_LABELS[p.role] || p.role)})</span>`;
                }

                const link = p.present
                    ? `<span class="status-badge ${p.state === 'up' ? 'badge-up' : 'badge-down'}">${p.state === 'up' ? 'Connected' : 'No link'}</span>`
                    : '<span class="status-badge badge-down">Missing</span>';

                return `<tr>
                    <td data-label="Port"><strong>${escapeHtml(p.name)}</strong></td>
                    <td data-label="Role">
                        <select class="role-select" data-port="${escapeHtml(p.name)}" aria-label="Role of ${escapeHtml(p.name)}"${editable && !saving ? '' : ' disabled'}>${options}</select>
                        ${note}
                    </td>
                    <td data-label="Link">${link}</td>
                    <td data-label="Speed">${escapeHtml(p.speed || '-')}</td>
                    <td data-label="MAC address"><code class="code-mac">${escapeHtml(p.mac || '-')}</code></td>
                    <td data-label="Driver">${p.driver ? `<span class="badge-driver">${escapeHtml(p.driver)}</span>` : '-'}</td>
                </tr>`;
            }).join('');
        }

        dirty = isDirty();
        const issue = dirty ? problem() : '';
        const hint = $('ports-hint');
        if (!editable) hint.textContent = 'A change is being applied or waits for confirmation: ports cannot be edited right now.';
        else hint.textContent = issue;
        hint.classList.toggle('form-hint--error', !!issue);
        $('ports-save').disabled = !editable || saving || !dirty || !!issue;
        $('ports-reset').disabled = saving || !dirty;
    }

    async function load() {
        const res = await apiFetch('/api/v1/ports');
        if (!res || !res.ok) {
            showMessage('Could not load the ports.', 'error');
            return;
        }
        const data = await res.json();
        const previous = ports;
        ports = data.ports || [];
        editable = data.editable !== false;

        // Keep what the person is choosing; new ports (hardware plugged in) start at the server's value
        const next = {};
        ports.forEach(p => {
            const known = previous.find(q => q.name === p.name);
            next[p.name] = (dirty && known && chosen[p.name] !== undefined) ? chosen[p.name] : p.role;
        });
        chosen = next;
        render();
    }

    async function save() {
        const problemText = problem();
        if (problemText) { showMessage(problemText, 'error'); return; }

        saving = true;
        render();
        const res = await apiFetch('/api/v1/ports', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ assignments: chosen }),
        });
        saving = false;

        if (!res) { showMessage('The router did not answer. Try again.', 'error'); render(); return; }
        if (!res.ok) {
            let body = {};
            try { body = await res.json(); } catch (e) { /* not JSON */ }
            const detail = (body.errors || []).map(e => e.message).join(' ');
            showMessage(detail || body.error || `Saving failed (${res.status}).`, 'error');
            render();
            return;
        }

        dirty = false;
        showMessage('Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        await load();
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    function wire() {
        if (wired) return;
        wired = true;

        $('ports-list').addEventListener('change', (e) => {
            const select = e.target.closest('select[data-port]');
            if (!select) return;
            const name = select.dataset.port;
            const role = select.value;

            // One WAN only: picking WAN here releases the port that had it
            if (role === 'wan') {
                ports.forEach(p => { if (p.name !== name && chosen[p.name] === 'wan') chosen[p.name] = 'unassigned'; });
            }
            chosen[name] = role;
            showMessage('');
            render();
        });
        $('ports-save').addEventListener('click', save);
        $('ports-reset').addEventListener('click', () => {
            chosen = serverRoles();
            showMessage('');
            render();
        });
        $('ports-refresh').addEventListener('click', load);
    }

    function init() {
        wire();
        dirty = false;
        showMessage('');
        load();
        // Link state changes while you look; never replace what the person is editing
        pollInterval = setInterval(() => { if (!dirty && !saving) load(); }, 5000);
        return () => { clearInterval(pollInterval); pollInterval = null; };
    }

    return { init };
})();
