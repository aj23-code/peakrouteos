// web/static/scripts/ports.js
// Ports page: pick the role of every network port (Internet/WAN, LAN or unassigned).

window.PortsModule = (function () {
    const ROLE_LABELS = { wan: 'Internet (WAN)', lan: 'LAN', unassigned: 'Unassigned' };

    let pollInterval = null;
    let ports = [];          // last answer from the server
    let editable = true;
    let chosen = {};         // port name -> role currently selected in the table
    let networks = [];       // what a LAN port can carry: { id, name, vlan, default }
    let chosenNets = {};     // port name -> { native, tagged } currently selected
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

    const serverNets = (p) => ({ native: p.native || 'lan', tagged: (p.tagged || []).slice() });
    const netsOf = (name) => chosenNets[name] || { native: 'lan', tagged: [] };
    const sameNets = (a, b) => a.native === b.native && a.tagged.slice().sort().join(',') === b.tagged.slice().sort().join(',');

    function isDirty() {
        const saved = serverRoles();
        return ports.some(p => chosen[p.name] !== saved[p.name] ||
            (chosen[p.name] === 'lan' && p.role === 'lan' && !sameNets(netsOf(p.name), serverNets(p))));
    }

    // Only worth showing when there is more than the default network to choose from
    const showNets = () => networks.length > 1;

    function netsHtml(p) {
        const cur = netsOf(p.name);
        const off = editable && !saving ? '' : ' disabled';
        const label = (n) => `${n.name} (VLAN ${n.vlan})`;
        const options = networks.map(n => `<option value="${escapeHtml(n.id)}"${n.id === cur.native ? ' selected' : ''}>${escapeHtml(label(n))}</option>`).join('');
        const tagged = networks.filter(n => !n.default && n.id !== cur.native).map(n =>
            `<label class="toggle toggle--small"><input type="checkbox" class="net-tagged" data-port="${escapeHtml(p.name)}" data-net="${escapeHtml(n.id)}"${cur.tagged.includes(n.id) ? ' checked' : ''}${off}> ${escapeHtml(label(n))}</label>`).join('');
        return `<div class="port-nets">
            <label class="port-net-row"><span class="form-hint">Untagged network</span>
                <select class="role-select net-native" data-port="${escapeHtml(p.name)}" aria-label="Untagged network of ${escapeHtml(p.name)}"${off}>${options}</select></label>
            ${tagged ? `<div class="port-net-row"><span class="form-hint">Also carries, tagged</span><span class="port-tagged">${tagged}</span></div>` : ''}
        </div>`;
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
                // a Wi-Fi adapter can only be taken off a role, never given one
                const choices = Object.keys(ROLE_LABELS).filter(r => !p.wireless || r === 'unassigned' || r === p.role);
                const options = choices.map(r =>
                    `<option value="${r}"${r === role ? ' selected' : ''}>${ROLE_LABELS[r]}</option>`).join('');

                let note = '';
                if (p.wireless) {
                    note = '<span class="tag tag-warn" title="Wi-Fi is set up on the Wireless page">Wi-Fi adapter</span>';
                } else if (!p.present) {
                    note = '<span class="tag tag-warn">Hardware missing</span>';
                } else if (p.pending && role === p.role) {
                    note = `<span class="tag tag-draft" title="In effect now: ${escapeHtml(ROLE_LABELS[p.active_role] || p.active_role)}">Draft</span>`;
                }
                if (role !== p.role) {
                    note = `<span class="tag tag-draft">Unsaved (was ${escapeHtml(ROLE_LABELS[p.role] || p.role)})</span>`;
                } else if (role === 'lan' && p.role === 'lan' && !sameNets(netsOf(p.name), serverNets(p))) {
                    note = '<span class="tag tag-draft">Unsaved networks</span>';
                }

                const link = p.present
                    ? `<span class="status-badge ${p.state === 'up' ? 'badge-up' : 'badge-down'}">${p.state === 'up' ? 'Connected' : 'No link'}</span>`
                    : '<span class="status-badge badge-down">Missing</span>';

                return `<tr>
                    <td data-label="Port"><strong>${escapeHtml(p.name)}</strong></td>
                    <td data-label="Role">
                        <select class="role-select" data-port="${escapeHtml(p.name)}" data-role aria-label="Role of ${escapeHtml(p.name)}"${editable && !saving ? '' : ' disabled'}>${options}</select>
                        ${note}
                        ${role === 'lan' && showNets() ? netsHtml(p) : ''}
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
        networks = data.networks || [];
        editable = data.editable !== false;

        // Keep what the person is choosing; new ports (hardware plugged in) start at the server's value
        const next = {};
        ports.forEach(p => {
            const known = previous.find(q => q.name === p.name);
            next[p.name] = (dirty && known && chosen[p.name] !== undefined) ? chosen[p.name] : p.role;
        });
        chosen = next;

        // the networks each LAN port carries follow the same rule: what is being edited stays
        const nextNets = {};
        ports.forEach(p => {
            const known = previous.find(q => q.name === p.name);
            nextNets[p.name] = (dirty && known && chosenNets[p.name]) ? chosenNets[p.name] : serverNets(p);
        });
        chosenNets = nextNets;
        render();
    }

    // The networks of every port that is a LAN port in the choice; the server ignores the default-only ones
    function networksToSend() {
        const out = {};
        ports.forEach(p => {
            if (chosen[p.name] !== 'lan') return;
            const c = netsOf(p.name);
            out[p.name] = { native: c.native === 'lan' ? '' : c.native, tagged: c.tagged };
        });
        return out;
    }

    async function save() {
        const problemText = problem();
        if (problemText) { showMessage(problemText, 'error'); return; }

        saving = true;
        render();
        const res = await apiFetch('/api/v1/ports', {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ assignments: chosen, networks: networksToSend() }),
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
            const netSelect = e.target.closest('select.net-native');
            if (netSelect) {
                const cur = netsOf(netSelect.dataset.port);
                // a network cannot be both the untagged and a tagged one on the same port
                chosenNets[netSelect.dataset.port] = { native: netSelect.value, tagged: cur.tagged.filter(t => t !== netSelect.value) };
                showMessage('');
                render();
                return;
            }
            const box = e.target.closest('input.net-tagged');
            if (box) {
                const cur = netsOf(box.dataset.port);
                const tagged = cur.tagged.filter(t => t !== box.dataset.net);
                if (box.checked) tagged.push(box.dataset.net);
                chosenNets[box.dataset.port] = { native: cur.native, tagged };
                showMessage('');
                render();
                return;
            }
            const select = e.target.closest('select[data-role]');
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
            chosenNets = {};
            ports.forEach(p => { chosenNets[p.name] = serverNets(p); });
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
