// web/static/scripts/dhcp.js

window.DHCPModule = (function () {
    let pollInterval = null;
    let networkFilter = '';   // "" = every network
    let wired = false;
    let editingMac = null;    // the device whose nickname is being typed: the table is not redrawn under the cursor
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

    // The first cell: the nickname (or the name the device gives itself), a pencil to change it, and while it is being changed an input.
    // The device's own name stays underneath a nickname, small, so that the two can be told apart.
    function nameCell(c) {
        const mac = escapeHtml(c.mac_address);
        if (editingMac === c.mac_address) {
            return `<td><div class="client-name-edit">
                <input type="text" class="client-name-input" data-name-input="${mac}" value="${escapeHtml(c.nickname || '')}" placeholder="${escapeHtml(c.hostname || 'Name this device')}" maxlength="40" autocomplete="off" spellcheck="false" aria-label="Nickname">
                <a href="#" class="view-all-link" data-name-save="${mac}">Save</a> <a href="#" class="view-all-link" data-name-cancel="${mac}">Cancel</a></div></td>`;
        }
        const shown = c.nickname || (c.static ? '' : c.hostname);
        let main = shown ? `<strong>${escapeHtml(shown)}</strong>` : '';
        if (c.static) main = `<span class="tag" title="This device has an address of its own: it did not ask this router for one">Static</span>` + (shown ? ` ${main}` : '');
        const sub = c.nickname && c.hostname && c.nickname !== c.hostname
            ? `<div class="form-hint" title="The name the device gives itself">${escapeHtml(c.hostname)}</div>` : '';
        const label = escapeHtml(shown || c.mac_address);
        return `<td>${main} <button type="button" class="client-rename" data-rename="${mac}" title="${c.nickname ? 'Change the nickname' : 'Give this device a nickname'}" aria-label="Rename ${label}">&#9998;</button>${sub}</td>`;
    }

    // a device that was seen on another network than its address says (the wrong VLAN, or a wrong address of its own)
    function seenOnHtml(c) {
        if (!c.seen_on) return '';
        return ` <span class="tag tag-draft" title="The router sees this device on ${escapeHtml(c.seen_on)}, but its address belongs to ${escapeHtml(c.network)}">Seen on ${escapeHtml(c.seen_on)}</span>`;
    }

    // whether the router keeps an address for the device, said in brackets after the address, with a link to reserve the one it has or to
    // give the reservation up. What is in the draft but not committed yet is said so: the list itself follows what is committed.
    function reservedHtml(c) {
        const note = (text) => ` <span class="form-hint">(${text})</span>`;
        const link = (attr, label, title) => `<div><a href="#" class="view-all-link" ${attr}="${escapeHtml(c.mac_address)}" title="${title}">${label}</a></div>`;
        const unreserve = link('data-unreserve', 'Unreserve', 'Stop keeping an address for this device');
        if (c.reserved) {
            const moves = c.reservation && c.reservation !== c.ip_address ? `<div class="form-hint">will move to ${escapeHtml(c.reservation)}</div>` : '';
            if (c.reserve_change === 'removed') return note('reserved, removed in draft');
            if (c.reserve_change === 'changed') return note('reserved, address changed in draft') + unreserve + moves;
            return note('reserved') + unreserve + moves;
        }
        if (c.reserve_change === 'added') return note('reserved, draft') + unreserve;
        return link('data-reserve', 'Reserve', 'Always give this device this address');
    }

    function showMessage(text, kind) {
        const box = document.getElementById('clients-message');
        if (!box) return;
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    // Starts, ends or saves the typing of a nickname. A nickname is only a label: it is saved at once, and nothing else changes.
    async function saveName(mac, name) {
        showMessage('');
        const res = await apiFetch('/api/v1/clients/' + encodeURIComponent(mac) + '/name', {
            method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ name }),
        });
        if (!res || !res.ok) {
            let err = {};
            try { err = res ? await res.json() : {}; } catch (e) { /* not JSON */ }
            showMessage(err.error || 'The nickname could not be saved.', 'error');
            return false;
        }
        return true;
    }

    // saves what is typed in the nickname box, then shows the table again with the new name
    async function finishName(body) {
        const input = body.querySelector('[data-name-input]');
        if (!input) return;
        const mac = input.dataset.nameInput;
        if (await saveName(mac, input.value)) {
            editingMac = null;
            await fetchAllLeases();
        }
    }

    // takes the device out of the draft's list of reserved addresses (nothing changes until it is committed)
    async function unreserve(mac) {
        showMessage('');
        const res = await apiFetch('/api/v1/dhcp/reservations');
        if (!res || !res.ok) { showMessage('The reserved addresses could not be read.', 'error'); return; }
        const list = ((await res.json()).reservations || []).filter(r => r.mac.toLowerCase() !== mac.toLowerCase());
        const put = await apiFetch('/api/v1/dhcp/reservations', {
            method: 'PUT', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ reservations: list.map(r => ({ mac: r.mac, ip: r.ip, name: r.name })) }),
        });
        if (!put || !put.ok) {
            let err = {};
            try { err = put ? await put.json() : {}; } catch (e) { /* not JSON */ }
            showMessage((err.errors || []).map(e => e.message).join(' ') || err.error || 'The reservation could not be removed.', 'error');
            return;
        }
        showMessage('Removed from the draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
        fetchAllLeases();
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
        draw((await res.json()) || [], false);
    }

    // Draws the table from a list. While a nickname is being typed the rows are left alone (they would be redrawn under the cursor every
    // three seconds); starting and ending the typing redraws them from the last list that was read (force).
    function draw(all, force) {
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

        if (!tbody || (editingMac && !force)) return;

        if (!leases || leases.length === 0) {
            tbody.innerHTML = `<tr><td colspan="7" class="table-placeholder">No devices have an address from this router yet.</td></tr>`;
            return;
        }

        tbody.innerHTML = leases.map(c => `
        <tr${c.status === 'offline' ? ' class="row-offline"' : ''}>
        <td>${statusHtml(c)}</td>
        ${nameCell(c)}
        <td>${escapeHtml(c.ip_address)}${reservedHtml(c)}</td>
        <td><code>${escapeHtml(c.mac_address)}</code></td>
        <td>${connectionHtml(c)}${c.network ? ` <span class="chip" title="The local network this device is on">${escapeHtml(c.network)}</span>` : ''}${seenOnHtml(c)}</td>
        <td>${escapeHtml(c.expiry_text)}</td>
        <td>${formatBytes(c.total_bytes || 0)}</td>
        </tr>
        `).join('');
    }

    function init() {
        editingMac = null;
        if (!wired) {
            wired = true;
            const select = document.getElementById('lease-network');
            if (select) select.addEventListener('change', () => { networkFilter = select.value; fetchAllLeases(); });
            const body = document.getElementById('all-clients-table-body');
            if (body) body.addEventListener('keydown', (e) => {
                if (!e.target.matches || !e.target.matches('[data-name-input]')) return;
                if (e.key === 'Enter') { e.preventDefault(); finishName(body); }
                if (e.key === 'Escape') { e.preventDefault(); editingMac = null; draw(lastLeases, true); }
            });
            if (body) body.addEventListener('click', (e) => {
                const rn = e.target.closest('[data-rename]');
                if (rn) {
                    e.preventDefault();
                    editingMac = rn.dataset.rename;
                    draw(lastLeases, true);
                    const input = body.querySelector('[data-name-input]');
                    if (input) { input.focus(); input.select(); }
                    return;
                }
                const cancel = e.target.closest('[data-name-cancel]');
                if (cancel) { e.preventDefault(); editingMac = null; draw(lastLeases, true); return; }
                const save = e.target.closest('[data-name-save]');
                if (save) { e.preventDefault(); finishName(body); return; }
                const un = e.target.closest('[data-unreserve]');
                if (un) { e.preventDefault(); unreserve(un.dataset.unreserve); return; }
                const link = e.target.closest('[data-reserve]');
                if (!link) return;
                e.preventDefault();
                const c = lastLeases.find(x => x.mac_address === link.dataset.reserve);
                if (c && window.DHCPServerModule) window.DHCPServerModule.reserve({ mac: c.mac_address, ip: c.ip_address, hostname: c.hostname, nickname: c.nickname });
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
