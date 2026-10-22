// web/static/scripts/dhcp.js

window.DHCPModule = (function () {
    let pollInterval = null;
    let networkFilter = '';   // "" = every network
    let wired = false;
    let lastLeases = [];      // what the table shows now, for the Reserve links
    let shownNetworks = '';   // the options in the filter now (rebuilt only when they change, or an open list would close every poll)

    // How the device is attached, as an icon: Wi-Fi, or an Ethernet plug, and the local network it is on (LAN, IoT, Guest...) beside it. The details (Wi-Fi name and band) are in
    // the tooltip and the accessible name; a dash means the Wi-Fi side could not be read, so a cable is not assumed.
    const ICON_WIFI = '<svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M2 8.8a15 15 0 0 1 20 0"/><path d="M5.5 12.4a10 10 0 0 1 13 0"/><path d="M9 16a5 5 0 0 1 6 0"/><circle cx="12" cy="19.5" r="0.9" fill="currentColor"/></svg>';
    const ICON_ETHERNET = '<svg viewBox="0 0 24 24" width="20" height="20" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M6 3h12v9a3 3 0 0 1-3 3H9a3 3 0 0 1-3-3z"/><path d="M10 15v3h4v-3"/><path d="M12 18v3"/><path d="M9.5 3v4M12 3v4M14.5 3v4"/></svg>';

    function connectionHtml(c) {
        if (c.connection === 'wifi') {
            const label = ['Wi-Fi', c.ssid, c.band].filter(Boolean).join(' \u00b7 ');
            return `<span class="conn-icon conn-icon--wifi" role="img" aria-label="${escapeHtml(label)}" title="${escapeHtml(label)}">${ICON_WIFI}</span>`;
        }
        if (c.connection === 'wired') {
            return `<span class="conn-icon" role="img" aria-label="Wired" title="Wired">${ICON_ETHERNET}</span>`;
        }
        return '<span class="form-hint" title="The Wi-Fi side could not be read" aria-label="Unknown">\u2013</span>';
    }

    // whether the router keeps an address for the device; if not, a link to reserve the one it has
    function reservedHtml(c) {
        if (c.reserved) {
            const moves = c.reservation && c.reservation !== c.ip_address
                ? `<div class="form-hint">will move to ${escapeHtml(c.reservation)}</div>` : '';
            return `<div><span class="tag" title="The router always gives this device the same address">Reserved</span></div>${moves}`;
        }
        return `<div><a href="#" class="view-all-link" data-reserve="${escapeHtml(c.mac_address)}" title="Always give this device this address">Reserve</a></div>`;
    }

    // "5 minutes ago" for a time in Unix seconds
    function ago(epoch) {
        const s = Math.max(0, Math.floor(Date.now() / 1000 - epoch));
        if (s < 60) return 'just now';
        const unit = (n, name) => `${n} ${name}${n === 1 ? '' : 's'} ago`;
        if (s < 3600) return unit(Math.floor(s / 60), 'minute');
        if (s < 86400) return unit(Math.floor(s / 3600), 'hour');
        return unit(Math.floor(s / 86400), 'day');
    }

    // Online or offline (the device may be gone while its lease is still held), with when it was last seen
    function statusHtml(c) {
        if (c.status === 'online') return '<span class="client-status client-status--online"><span class="status-pip"></span>Online</span>';
        if (c.status === 'offline') {
            const seen = c.last_seen_epoch > 0 ? `Last seen ${ago(c.last_seen_epoch)}` : 'Not seen yet';
            return `<span class="client-status"><span class="status-pip"></span>Offline</span><div class="form-hint">${escapeHtml(seen)}</div>`;
        }
        return '<span class="form-hint" title="Whether this device is here cannot be told on this system">\u2013</span>';
    }

    async function fetchAllLeases() {
        const res = await apiFetch('/api/v1/dhcp/leases');
        if (!res || !res.ok) return;

        const all = (await res.json()) || [];
        // the filter lists the networks that have a device; one that no longer has any goes back to "all"
        const networks = [...new Set(all.map(c => c.network).filter(Boolean))].sort();
        if (networkFilter && !networks.includes(networkFilter)) networkFilter = '';
        const picker = document.getElementById('lease-network-picker');
        const select = document.getElementById('lease-network');
        if (picker && select) {
            picker.classList.toggle('hidden', networks.length < 2);
            if (shownNetworks !== networks.join('\n')) {
                shownNetworks = networks.join('\n');
                select.innerHTML = '<option value="">All networks</option>' +
                    networks.map(n => `<option value="${escapeHtml(n)}">${escapeHtml(n)}</option>`).join('');
            }
            select.value = networkFilter;
        }
        lastLeases = all;
        const leases = networkFilter ? all.filter(c => c.network === networkFilter) : all;
        // devices that are here come first
        leases.sort((a, b) => (b.status === 'online') - (a.status === 'online'));
        const tbody = document.getElementById('all-clients-table-body');
        const badge = document.getElementById('lease-count-badge');

        if (badge) {
            const known = leases.some(c => c.status === 'online' || c.status === 'offline');
            const online = leases.filter(c => c.status === 'online').length;
            const offline = leases.length - online;
            badge.innerHTML = known
                ? `<span class="lease-count-online">${online} online</span><span class="lease-count-sep">\u00b7</span><span class="lease-count-offline">${offline} offline</span>`
                : `${leases.length} ${leases.length === 1 ? 'device' : 'devices'}`;
        }

        if (!tbody) return;

        if (!leases || leases.length === 0) {
            tbody.innerHTML = `<tr><td colspan="7" class="table-placeholder">No devices have an address from this router yet.</td></tr>`;
            return;
        }

        tbody.innerHTML = leases.map(c => `
        <tr${c.status === 'offline' ? ' class="row-offline"' : ''}>
        <td>${statusHtml(c)}</td>
        <td><strong>${escapeHtml(c.hostname)}</strong></td>
        <td>${escapeHtml(c.ip_address)}${reservedHtml(c)}</td>
        <td><code>${escapeHtml(c.mac_address)}</code></td>
        <td>${connectionHtml(c)}${c.network ? ` <span class="chip" title="The local network this device is on">${escapeHtml(c.network)}</span>` : ''}</td>
        <td>${escapeHtml(c.expiry_text)}</td>
        <td>${formatBytes(c.total_bytes || 0)}</td>
        </tr>
        `).join('');
    }

    function init() {
        if (!wired) {
            wired = true;
            const select = document.getElementById('lease-network');
            if (select) select.addEventListener('change', () => { networkFilter = select.value; fetchAllLeases(); });
            const body = document.getElementById('all-clients-table-body');
            if (body) body.addEventListener('click', (e) => {
                const link = e.target.closest('[data-reserve]');
                if (!link) return;
                e.preventDefault();
                const c = lastLeases.find(x => x.mac_address === link.dataset.reserve);
                if (c && window.DHCPServerModule) window.DHCPServerModule.reserve({ mac: c.mac_address, ip: c.ip_address, hostname: c.hostname });
            });
        }
        fetchAllLeases();
        // Polls full lease table every 3 seconds while active
        pollInterval = setInterval(fetchAllLeases, 3000);

        // Return cleanup hook called by app.js when leaving the view
        return function destroy() {
            if (pollInterval) {
                clearInterval(pollInterval);
                pollInterval = null;
            }
        };
    }

    return { init };
})();
