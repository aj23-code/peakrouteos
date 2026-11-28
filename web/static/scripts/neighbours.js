// web/static/scripts/neighbours.js
// The Neighbours card of the Ports page: the switch for LLDP and what lldpd has heard on the LAN ports (docs/device-pages.md, phase 2).

window.NeighboursCard = (function () {
    let data = null;
    let timer = null;
    let wired = false;
    let busy = false;

    const $ = (id) => document.getElementById(id);

    function render() {
        if (!data) return;
        const box = $('lldp-enabled');
        const lock = !data.editable || busy;
        box.disabled = lock;
        $('lldp-draft').classList.toggle('hidden', !data.pending);
        const st = $('lldp-state');
        st.textContent = !data.active ? 'Off' : data.running ? 'On' : 'Starting…';
        st.className = 'tag ' + (data.active && data.running ? 'tag-ok' : '');
        const dirty = box.checked !== !!data.enabled;
        $('lldp-save').disabled = lock || !dirty;
        $('lldp-reset').disabled = busy || !dirty;
        $('lldp-hint').textContent = dirty ? 'You have unsaved changes.' : (!data.editable ? 'A change is being applied or waits for confirmation.' : '');

        const list = $('lldp-list');
        const ns = data.neighbours || [];
        $('lldp-table-wrap').classList.toggle('hidden', !ns.length);
        list.innerHTML = ns.map(n => {
            const kind = (n.capabilities || []).join(', ') || (n.via || '');
            const addr = (n.mgmt_ips || []).filter(a => a.indexOf(':') < 0).join(', ') || (n.mgmt_ips || []).join(', ');
            const name = n.name || n.chassis_id || '–';
            return `<tr><td><code>${escapeHtml(n.local_port)}</code></td>
                <td><strong>${escapeHtml(name)}</strong>${n.description ? `<br><span class="form-hint">${escapeHtml(n.description)}</span>` : ''}</td>
                <td>${escapeHtml(n.port_id || '–')}${n.port_desc ? `<br><span class="form-hint">${escapeHtml(n.port_desc)}</span>` : ''}${n.vlan ? ` <span class="form-hint">VLAN ${escapeHtml(n.vlan)}</span>` : ''}</td>
                <td>${addr ? `<code>${escapeHtml(addr)}</code>` : '–'}</td><td>${escapeHtml(kind)}</td></tr>`;
        }).join('');
        $('lldp-empty').textContent = !data.active
            ? (data.pending ? 'Commit the draft to start listening.' : 'Switch it on to see what is plugged into the LAN ports.')
            : !data.running ? 'The neighbour service is starting.'
            : ns.length ? '' : 'Nothing has announced itself on the LAN ports yet. Switches and access points send an announcement every 30 seconds when their own LLDP setting is on.';
    }

    async function load() {
        const res = await apiFetch('/api/v1/lldp');
        if (!res || !res.ok) return;
        const box = $('lldp-enabled');
        const edited = !!data && box.checked !== !!data.enabled; // what is being changed is left alone
        data = await res.json();
        if (!edited) box.checked = !!data.enabled;
        render();
    }

    async function save() {
        busy = true; render();
        const res = await apiFetch('/api/v1/lldp', {
            method: 'PUT', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ enabled: $('lldp-enabled').checked }),
        });
        busy = false;
        if (res && res.ok) {
            data = await res.json();
            $('lldp-enabled').checked = !!data.enabled;
            if (window.ConfigBanner) window.ConfigBanner.refresh();
        }
        render();
    }

    function wire() {
        if (wired) return;
        wired = true;
        $('lldp-enabled').addEventListener('change', render);
        $('lldp-save').addEventListener('click', save);
        $('lldp-reset').addEventListener('click', () => { if (data) { $('lldp-enabled').checked = !!data.enabled; render(); } });
    }

    function init() {
        wire();
        data = null; busy = false;
        load();
        timer = setInterval(() => { if (!busy) load(); }, 15000);
        return () => { clearInterval(timer); timer = null; };
    }

    return { init };
})();
