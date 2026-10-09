// web/static/scripts/networks.js
// Local Networks page: the router address and DHCP server of each network (today: the LAN).

window.NetworksModule = (function () {
    let network = null;      // last answer from the server
    let editable = true;
    let saving = false;
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const FIELD_INPUTS = {
        'ip': 'net-ip',
        'netmask': 'net-netmask',
        'dhcp.range_start': 'net-dhcp-start',
        'dhcp.range_end': 'net-dhcp-end',
        'dhcp.lease_time': 'net-dhcp-lease',
    };

    function showMessage(text, kind) {
        const box = $('net-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    function clearFieldErrors() {
        document.querySelectorAll('#net-card [data-error]').forEach(p => {
            p.textContent = '';
            p.classList.add('hidden');
        });
        Object.values(FIELD_INPUTS).forEach(id => $(id).removeAttribute('aria-invalid'));
    }

    function showFieldErrors(errors) {
        const leftovers = [];
        (errors || []).forEach(e => {
            const target = document.querySelector(`#net-card [data-error="${e.field}"]`);
            if (!target) { leftovers.push(e.message); return; }
            target.textContent = e.message;
            target.classList.remove('hidden');
            const input = $(FIELD_INPUTS[e.field]);
            if (input) input.setAttribute('aria-invalid', 'true');
        });
        return leftovers;
    }

    // What the form says right now, in the shape the API takes
    function formValues() {
        const dhcp = {
            enabled: $('net-dhcp-enabled').checked,
            range_start: $('net-dhcp-start').value.trim(),
            range_end: $('net-dhcp-end').value.trim(),
            lease_time: $('net-dhcp-lease').value.trim(),
        };
        return { ip: $('net-ip').value.trim(), netmask: $('net-netmask').value.trim(), dhcp };
    }

    function fillForm(n) {
        $('net-ip').value = n.ip || '';
        $('net-netmask').value = n.netmask || '';
        const d = n.dhcp || { enabled: false, range_start: '', range_end: '', lease_time: '' };
        $('net-dhcp-enabled').checked = !!d.enabled;
        $('net-dhcp-start').value = d.range_start || '';
        $('net-dhcp-end').value = d.range_end || '';
        $('net-dhcp-lease').value = d.lease_time || '';
    }

    // Is the form different from what the server has?
    function isDirty() {
        if (!network) return false;
        const v = formValues();
        const d = network.dhcp;
        if (v.ip !== (network.ip || '') || v.netmask !== (network.netmask || '')) return true;
        if (!d) {
            // not managed yet: untouched means switched off and empty
            return v.dhcp.enabled || v.dhcp.range_start || v.dhcp.range_end || v.dhcp.lease_time;
        }
        return v.dhcp.enabled !== d.enabled || v.dhcp.range_start !== d.range_start ||
            v.dhcp.range_end !== d.range_end || v.dhcp.lease_time !== d.lease_time;
    }

    function render() {
        if (!network) return;
        $('net-name').textContent = network.name || 'LAN';
        $('net-draft').classList.toggle('hidden', !network.pending);

        const chips = $('net-ports');
        chips.innerHTML = (network.ports || []).length
            ? network.ports.map(p => `<span class="chip">${escapeHtml(p)}</span>`).join('') +
              (network.bridged ? '<span class="form-hint">&nbsp;bridged together</span>' : '')
            : '<span class="form-hint">No ports yet.</span>';

        const dhcpOn = $('net-dhcp-enabled').checked;
        $('net-dhcp-fields').classList.toggle('is-off', !dhcpOn);
        ['net-dhcp-start', 'net-dhcp-end', 'net-dhcp-lease'].forEach(id => { $(id).disabled = !dhcpOn || !editable || saving; });
        ['net-ip', 'net-netmask', 'net-dhcp-enabled'].forEach(id => { $(id).disabled = !editable || saving; });
        $('net-dhcp-note').classList.toggle('hidden', network.dhcp !== null || dhcpOn);

        const dirty = isDirty();
        $('net-hint').textContent = !editable
            ? 'A change is being applied or waits for confirmation: networks cannot be edited right now.'
            : (dirty ? 'You have unsaved changes.' : '');
        $('net-save').disabled = !editable || saving || !dirty;
        $('net-reset').disabled = saving || !dirty;
    }

    async function load(keepEdits) {
        const res = await apiFetch('/api/v1/networks');
        if (!res || !res.ok) { showMessage('Could not load the networks.', 'error'); return; }
        const data = await res.json();
        editable = data.editable !== false;
        network = (data.networks || []).find(n => n.id === 'lan') || (data.networks || [])[0] || null;
        if (!network) { showMessage('No network is configured yet.', 'error'); return; }
        if (!keepEdits) fillForm(network);
        render();
    }

    async function save() {
        clearFieldErrors();
        showMessage('');
        const v = formValues();
        const body = { ip: v.ip, netmask: v.netmask };
        // A network the configuration does not manage yet and the person left alone stays that way
        if (!(network.dhcp === null && !isDhcpTouched(v.dhcp))) body.dhcp = v.dhcp;

        saving = true;
        render();
        const res = await apiFetch('/api/v1/networks/' + encodeURIComponent(network.id), {
            method: 'PUT',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify(body),
        });
        saving = false;

        if (!res) { showMessage('The router did not answer. Try again.', 'error'); render(); return; }
        if (!res.ok) {
            let err = {};
            try { err = await res.json(); } catch (e) { /* not JSON */ }
            const leftovers = showFieldErrors(err.errors);
            if (!err.errors || leftovers.length || err.errors.length === 0) {
                showMessage(leftovers.join(' ') || err.error || `Saving failed (${res.status}).`, 'error');
            } else {
                showMessage('Some settings need attention.', 'error');
            }
            render();
            return;
        }

        showMessage('Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        await load(false);
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    function isDhcpTouched(d) {
        return d.enabled || d.range_start || d.range_end || d.lease_time;
    }

    function wire() {
        if (wired) return;
        wired = true;
        $('net-card').addEventListener('input', () => render());
        $('net-card').addEventListener('change', () => render());
        $('net-save').addEventListener('click', save);
        $('net-reset').addEventListener('click', () => {
            clearFieldErrors();
            showMessage('');
            if (network) fillForm(network);
            render();
        });
        // Pressing Enter in a field saves
        $('net-card').addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && e.target.tagName === 'INPUT' && e.target.type === 'text' && !$('net-save').disabled) {
                e.preventDefault();
                save();
            }
        });
    }

    function init() {
        wire();
        clearFieldErrors();
        showMessage('');
        network = null;
        load(false);
        return () => {};
    }

    return { init };
})();
