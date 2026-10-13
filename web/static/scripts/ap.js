// web/static/scripts/ap.js
// Operation mode: whether this device is a router or an access point, and the access point's network (the cable ports that join the
// bridge, and how the device gets its own address). Saving only writes a draft. docs/access-point.md.

window.APModule = (function () {
    let data = null;                // GET /api/v1/ap
    let form = null;                // what is typed: { mode, ports, addrMode, ip, netmask, gateway, dns }
    let busy = false;
    let message = null;
    let errors = {};
    let wired = false;

    const $ = (id) => document.getElementById(id);

    function formOf(d) {
        return { mode: d.mode, ports: d.ap.ports.slice(), addrMode: d.ap.mode || 'dhcp', ip: d.ap.ip || '', netmask: d.ap.netmask || '255.255.255.0',
            gateway: d.ap.gateway || '', dns: (d.ap.dns || []).join(', ') };
    }

    const norm = (f) => JSON.stringify([f.mode, f.mode === 'ap' ? f.ports.slice().sort() : [], f.mode === 'ap' ? f.addrMode : '',
        f.mode === 'ap' && f.addrMode === 'static' ? [f.ip, f.netmask, f.gateway, f.dns] : []]);
    const dirty = () => norm(form) !== norm(formOf(data));

    function err(field) { return errors[field] ? `<p class="field-error">${escapeHtml(errors[field])}</p>` : ''; }

    function render() {
        if (!data) return;
        const lock = busy || !data.editable;
        const ap = form.mode === 'ap';
        const input = (field, label, value, ph) => `<div class="form-field"><label for="ap-${field}">${label}</label>
            <input type="text" id="ap-${field}" data-ap="${field}" value="${escapeHtml(value)}" placeholder="${escapeHtml(ph || '')}" autocomplete="off" spellcheck="false"${errors[field] ? ' aria-invalid="true"' : ''}${lock ? ' disabled' : ''}>${err(field)}</div>`;
        const pending = data.mode !== data.active_mode;
        let html = `<section class="card">
            <div class="net-card-head"><h2>What this device is</h2>${pending ? '<span class="tag tag-draft">Draft</span>' : ''}<span class="tag tag-ok">Now: ${data.active_mode === 'ap' ? 'access point' : 'router'}</span></div>
            <div class="net-access">
                <label class="toggle toggle--block"><input type="radio" name="ap-mode" data-ap="mode" value="router"${!ap ? ' checked' : ''}${lock ? ' disabled' : ''}>
                    <span><strong>Router</strong><span class="form-hint">Connects to the internet, makes the local networks, gives out addresses, filters traffic. This is the usual way.</span></span></label>
                <label class="toggle toggle--block"><input type="radio" name="ap-mode" data-ap="mode" value="ap"${ap ? ' checked' : ''}${lock ? ' disabled' : ''}>
                    <span><strong>Access point</strong><span class="form-hint">Joins your existing network: its cable ports and Wi-Fi are all part of that one network, which another router runs. No internet connection, addresses, firewall or VPN of its own; the settings of a router are kept, unused, in case you go back.</span></span></label>
            </div>
            ${ap ? '' : '<p class="form-hint">Changing a router into an access point disconnects everything that depends on it as a router: devices lose their addresses and the internet until the other router takes over.</p>'}
        </section>`;

        if (ap) {
            const ports = data.ports.length ? data.ports.map(p => `<label class="toggle toggle--block"><input type="checkbox" data-ap-port="${escapeHtml(p.name)}"${form.ports.includes(p.name) ? ' checked' : ''}${lock ? ' disabled' : ''}>
                <span><strong>${escapeHtml(p.name)}</strong><span class="form-hint">${escapeHtml(p.mac)}${p.state === 'up' ? ', cable plugged in' : ''}</span></span></label>`).join('') : '<p class="form-hint">No cable port was found. Wi-Fi can still join the network, and a wireless link (Wireless page) can carry the access point.</p>';
            html += `<section class="card">
                <div class="net-card-head"><h2>The access point's network</h2></div>
                <h3 class="form-section">Cable ports</h3>
                <p class="form-hint">Every port you choose is part of the one network; plug the other router (or a switch that leads to it) into any of them.</p>
                <div class="net-access">${ports}</div>${err('ports')}
                <h3 class="form-section">The device's own address</h3>
                <div class="net-access">
                    <label class="toggle toggle--block"><input type="radio" name="ap-addr" data-ap="addrMode" value="dhcp"${form.addrMode === 'dhcp' ? ' checked' : ''}${lock ? ' disabled' : ''}>
                        <span><strong>From the other router (DHCP)</strong><span class="form-hint">The other router lists the device under its host name (System, Settings): look for it in that router's list of devices to find the address.</span></span></label>
                    <label class="toggle toggle--block"><input type="radio" name="ap-addr" data-ap="addrMode" value="static"${form.addrMode === 'static' ? ' checked' : ''}${lock ? ' disabled' : ''}>
                        <span><strong>A fixed address</strong><span class="form-hint">Easier to find again. Choose one outside the range the other router hands out.</span></span></label>
                </div>
                ${form.addrMode === 'static' ? `<div class="form-grid">${input('ip', 'Address', form.ip, '192.168.1.2')}${input('netmask', 'Netmask', form.netmask, '255.255.255.0')}
                    ${input('gateway', 'Gateway (the other router)', form.gateway, '192.168.1.1')}${input('dns', 'DNS servers', form.dns, '192.168.1.1')}</div>` : ''}
                <div class="wiz-note">While you set the device up, the address <strong>${escapeHtml(data.recovery_address.split('/')[0])}</strong> keeps working beside its own: a computer set to an address in ${escapeHtml(data.recovery_address)} reaches the device there even if you do not know the address it was given.</div>
            </section>`;
        }

        html += `${message ? `<div class="form-message${message.kind === 'ok' ? ' form-message--ok' : ''}" role="alert">${escapeHtml(message.text)}</div>` : ''}
            <div class="form-actions">
                <span class="form-hint">${!data.editable ? 'A change is being applied or waits for confirmation.' : (dirty() ? 'You have unsaved changes.' : '')}</span>
                <button type="button" class="cfg-btn cfg-btn--ghost" id="ap-reset"${busy || !dirty() ? ' disabled' : ''}>Reset</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="ap-save"${lock || !dirty() ? ' disabled' : ''}>Save to draft</button>
            </div>`;
        $('ap-body').innerHTML = html;
    }

    async function load() {
        const res = await apiFetch('/api/v1/ap');
        if (!res || !res.ok) { $('ap-body').innerHTML = '<div class="form-message" role="alert">Could not load the settings.</div>'; return; }
        data = await res.json();
        form = formOf(data);
        errors = {};
        render();
    }

    async function save() {
        busy = true; message = null; errors = {};
        render();
        const body = { mode: form.mode };
        if (form.mode === 'ap') {
            body.ports = form.ports;
            body.address_mode = form.addrMode;
            if (form.addrMode === 'static') {
                body.ip = form.ip; body.netmask = form.netmask; body.gateway = form.gateway;
                body.dns = form.dns.split(/[\s,]+/).filter(Boolean);
            }
        }
        const res = await apiFetch('/api/v1/ap', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        busy = false;
        if (!res || !res.ok) {
            let e = {};
            try { e = await res.json(); } catch (x) { /* not JSON */ }
            const general = [];
            (e.errors || []).forEach(x => { if (['ports', 'ip', 'netmask', 'gateway', 'dns', 'mode'].includes(x.field)) errors[x.field] = x.message; else general.push(x.message); });
            message = { text: general.join(' ') || (Object.keys(errors).length ? 'Some settings need attention.' : (e.error || 'Saving failed.')), kind: 'error' };
            render();
            return;
        }
        data = await res.json();
        form = formOf(data);
        message = { text: 'Saved as a draft. Review and commit it from the bar at the top when you are ready.', kind: 'ok' };
        render();
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('ap-body');
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (!form) return;
            if (el.dataset.apPort) {
                form.ports = form.ports.filter(p => p !== el.dataset.apPort);
                if (el.checked) form.ports.push(el.dataset.apPort);
                delete errors.ports; message = null;
                render();
                return;
            }
            if (el.dataset.ap === 'mode' || el.dataset.ap === 'addrMode') {
                form[el.dataset.ap] = el.value;
                if (el.dataset.ap === 'mode' && el.value === 'ap' && !form.ports.length) form.ports = data.ports.map(p => p.name);
                errors = {}; message = null;
                render();
            }
        });
        body.addEventListener('input', (e) => {
            const el = e.target;
            if (!form || !el.dataset.ap || el.type === 'radio') return;
            form[el.dataset.ap] = el.value;
            delete errors[el.dataset.ap];
            el.removeAttribute('aria-invalid');
            const msg = el.closest('.form-field').querySelector('.field-error');
            if (msg) msg.remove();
            const save = $('ap-save'), reset = $('ap-reset');
            if (save) save.disabled = busy || !data.editable || !dirty();
            if (reset) reset.disabled = busy || !dirty();
        });
        body.addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            if (btn.id === 'ap-save') save();
            else if (btn.id === 'ap-reset') { form = formOf(data); errors = {}; message = null; render(); }
        });
    }

    function init() {
        wire();
        data = null; form = null; message = null; errors = {};
        $('ap-body').innerHTML = '<p class="form-hint">Loading…</p>';
        load();
        return () => {};
    }

    return { init };
})();
