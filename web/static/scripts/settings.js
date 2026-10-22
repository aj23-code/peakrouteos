// web/static/scripts/settings.js
// Settings page: choices that apply to the whole router: its host name, country and local domain (General) and the time
// zone and time servers (Time). Saving only writes a draft. docs/settings.md describes them.

window.SettingsModule = (function () {
    let settings = null;
    // what is typed or chosen in each box (null: what the router has)
    let typed = blankTyped();
    let busy = false;
    let wired = false;
    let clockTimer = null;
    let power = { arming: null, state: null };   // arming: the button pressed once; state: what is happening after it was confirmed
    let powerTimer = null;
    // the factory reset: open (the password box is shown), the password as typed, an error, and what is happening once it was confirmed
    let reset = { open: false, password: '', error: '', done: false };

    const $ = (id) => document.getElementById(id);

    function blankTyped() { return { hostname: null, domain: null, send: null, tz: null, ntp: null, servers: null, syslog: null }; }

    function showMessage(text, kind) {
        const box = $('settings-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    const option = (value, label, current) =>
        `<option value="${escapeHtml(value)}"${value === current ? ' selected' : ''}>${escapeHtml(label)}</option>`;
    const draftTag = (on) => (on ? '<span class="tag tag-draft">Draft</span>' : '');
    const locked = () => busy || settings.editable === false;

    // ---------------------------------------------------------------- what the boxes show and whether they changed

    const shown = (k) => (typed[k] !== null ? typed[k] : settings[k] || '');
    const sendNow = () => (typed.send !== null ? typed.send : !!settings.send_hostname);
    const tzNow = () => (typed.tz !== null ? typed.tz : settings.timezone || '');
    const ntpNow = () => (typed.ntp !== null ? typed.ntp : !!settings.ntp_enabled);
    const serversNow = () => (typed.servers !== null ? typed.servers : (settings.ntp_servers || []).join(', '));
    const serverList = (text) => text.split(/[\s,]+/).filter(Boolean);

    // syslog forwarding: the working copy is {servers: [{host, port (text), protocol}], system, firewall}
    const syslogSaved = () => {
        const s = settings.syslog || { servers: [], system: false, firewall: false };
        return { servers: s.servers.map(x => ({ host: x.host, port: String(x.port), protocol: x.protocol })), system: !!s.system, firewall: !!s.firewall };
    };
    const syslogNow = () => typed.syslog || syslogSaved();
    const syslogEdit = () => { if (!typed.syslog) typed.syslog = syslogSaved(); return typed.syslog; };
    const syslogBody = () => {
        const s = syslogNow();
        return { servers: s.servers.filter(x => x.host.trim() || x.port.trim()).map(x => ({ host: x.host.trim(), port: x.port.trim() === '' ? 0 : Number(x.port.trim()), protocol: x.protocol })), system: s.system, firewall: s.firewall };
    };

    // the two settings that are a line of text: where they are saved, and what is sent
    const TEXT = {
        hostname: {
            url: '/api/v1/settings/hostname', fail: 'The host name could not be saved.', tidy: (v) => v.trim(),
            body: () => ({ hostname: TEXT.hostname.tidy(shown('hostname')), send_hostname: sendNow() }),
        },
        domain: {
            url: '/api/v1/settings/domain', fail: 'The domain could not be saved.', tidy: (v) => v.trim().toLowerCase(),
            body: () => ({ domain: TEXT.domain.tidy(shown('domain')) }),
        },
    };
    const keys = Object.keys(TEXT);

    function dirty(k) {
        if (!settings) return false;
        if (k === 'time') {
            return (typed.tz !== null && typed.tz !== (settings.timezone || '')) ||
                (typed.ntp !== null && typed.ntp !== !!settings.ntp_enabled) ||
                (typed.servers !== null && serverList(typed.servers).join(',') !== (settings.ntp_servers || []).join(','));
        }
        if (k === 'syslog') return typed.syslog !== null && JSON.stringify(typed.syslog) !== JSON.stringify(syslogSaved());
        const text = typed[k] !== null && TEXT[k].tidy(typed[k]) !== (settings[k] || '');
        return text || (k === 'hostname' && typed.send !== null && typed.send !== !!settings.send_hostname);
    }

    function updateButton(k) {
        const b = $(`settings-${k}-save`);
        if (b) b.disabled = busy || settings.editable === false || !dirty(k);
    }

    // ---------------------------------------------------------------- cards

    function textCard(k, title, label, placeholder, pending, hint, extra) {
        return `<section class="card">
            <div class="net-card-head"><h2>${title}</h2>${draftTag(pending)}</div>
            <div class="form-grid"><div class="form-field"><label for="settings-${k}">${label}</label>
                <input type="text" id="settings-${k}" value="${escapeHtml(shown(k))}" placeholder="${placeholder}" autocomplete="off" spellcheck="false" autocapitalize="off"${locked() ? ' disabled' : ''}>
                <p class="field-error hidden" id="settings-${k}-error"></p></div></div>
            <p class="form-hint">${hint}</p>
            ${extra || ''}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--primary" id="settings-${k}-save" disabled>Save to draft</button>
            </div>
        </section>`;
    }

    function zoneOptions() {
        const zones = settings.timezones || [];
        const current = tzNow();
        const live = settings.time && settings.time.timezone;
        let html = option('', live ? `Not set (the machine keeps its own: ${live})` : 'Not set (the machine keeps its own)', current);
        const suggested = settings.suggested_timezones || [];
        if (suggested.length) {
            html += `<optgroup label="Suggested for ${escapeHtml(countryName())}">${suggested.map(z => option(z, z.replace(/_/g, ' '), current)).join('')}</optgroup>`;
        }
        const groups = {};
        zones.forEach(z => { const a = z.name.includes('/') ? z.name.split('/')[0] : 'Other'; (groups[a] = groups[a] || []).push(z.name); });
        Object.keys(groups).sort().forEach(a => {
            html += `<optgroup label="${escapeHtml(a)}">${groups[a].map(z => option(z, z.replace(/_/g, ' '), current)).join('')}</optgroup>`;
        });
        return html;
    }

    function countryName() {
        const c = (settings.countries || []).find(x => x.code === settings.country);
        return c ? c.name : 'your country';
    }

    function timeStatusText() {
        const t = settings.time;
        if (!t) return 'The router cannot tell right now whether its clock is set from a time server.';
        if (!t.ntp_enabled) return 'The time service is switched off: the clock is not being kept right.';
        if (t.synchronized) return `The clock is set from a time server${t.server ? ` (${t.server})` : ''}.`;
        return 'The clock has not been set from a time server yet. That takes a minute or so after the internet connection comes up.';
    }

    function timeCard() {
        const on = ntpNow();
        return `<section class="card" id="settings-time">
            <div class="net-card-head"><h2>Time</h2>${draftTag(settings.time_pending)}</div>
            ${settings.time ? `<p class="settings-clock"><span class="form-hint">The router&rsquo;s clock</span> <strong id="settings-clock"></strong></p>` : ''}
            <p class="form-hint" id="settings-time-status">${escapeHtml(timeStatusText())}</p>
            <div class="form-grid"><div class="form-field"><label for="settings-tz">Time zone</label>
                <select id="settings-tz" class="role-select"${locked() ? ' disabled' : ''}>${zoneOptions()}</select>
                <p class="field-error hidden" id="settings-tz-error"></p></div></div>
            <label class="toggle toggle--block settings-send"><input type="checkbox" id="settings-ntp"${on ? ' checked' : ''}${locked() ? ' disabled' : ''}><span><strong>Keep the clock right with time servers (NTP)</strong><span class="form-hint">A router has no battery-backed clock it can trust: firewall logs, certificates and DHCP leases all depend on the time being right.</span></span></label>
            <div class="form-grid${on ? '' : ' hidden'}" id="settings-servers-grid"><div class="form-field"><label for="settings-servers">Time servers</label>
                <input type="text" id="settings-servers" value="${escapeHtml(serversNow())}" placeholder="${escapeHtml((settings.ntp_default_servers || []).join(', '))}" autocomplete="off" spellcheck="false" autocapitalize="off"${locked() ? ' disabled' : ''}>
                <p class="form-hint">Up to four, separated by commas: names such as <code>time.cloudflare.com</code> or addresses. Empty: <code>${escapeHtml((settings.ntp_default_servers || []).join(', '))}</code>.</p>
                <p class="field-error hidden" id="settings-servers-error"></p></div></div>
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--primary" id="settings-time-save" disabled>Save to draft</button></div>
        </section>`;
    }

    function syslogRows() {
        const s = syslogNow();
        return s.servers.map((x, i) => `<div class="settings-sl-row" data-i="${i}">
            <div class="form-field settings-sl-host"><label for="sl-host-${i}">Server</label>
                <input type="text" id="sl-host-${i}" data-sl="host" data-i="${i}" value="${escapeHtml(x.host)}" placeholder="logs.example.com" autocomplete="off" spellcheck="false" autocapitalize="off"${locked() ? ' disabled' : ''}></div>
            <div class="form-field settings-sl-port"><label for="sl-port-${i}">Port</label>
                <input type="text" id="sl-port-${i}" data-sl="port" data-i="${i}" value="${escapeHtml(x.port)}" placeholder="514" inputmode="numeric" autocomplete="off"${locked() ? ' disabled' : ''}></div>
            <div class="form-field settings-sl-proto"><label for="sl-proto-${i}">Protocol</label>
                <select id="sl-proto-${i}" class="role-select" data-sl="protocol" data-i="${i}"${locked() ? ' disabled' : ''}><option value="udp"${x.protocol === 'udp' ? ' selected' : ''}>UDP</option><option value="tcp"${x.protocol === 'tcp' ? ' selected' : ''}>TCP</option></select></div>
            <button type="button" class="cfg-btn cfg-btn--ghost settings-sl-remove" data-i="${i}"${locked() ? ' disabled' : ''} aria-label="Remove this server">Remove</button>
            <p class="field-error hidden settings-sl-error" id="sl-error-${i}"></p></div>`).join('');
    }

    function syslogStatusHtml() {
        const st = settings.syslog_status || [];
        if (!st.length) return '';
        const rows = st.map(x => {
            const label = `${escapeHtml(x.host)}:${x.port} (${x.protocol.toUpperCase()})`;
            const detail = x.last_error ? `<span class="tag tag-warn">${escapeHtml(x.last_error)}</span>`
                : (x.protocol === 'tcp' ? (x.connected ? '<span class="tag tag-ok">connected</span>' : '<span class="tag">not connected</span>') : '');
            return `<div class="settings-sl-status"><strong>${label}</strong> ${detail} <span class="form-hint">${x.sent.toLocaleString()} sent${x.dropped ? `, ${x.dropped.toLocaleString()} dropped (the server could not be reached and the queue was full)` : ''}</span></div>`;
        }).join('');
        return `<div class="settings-sl-statuses"><span class="form-hint">Running now</span>${rows}</div>`;
    }

    function syslogCard() {
        const s = syslogNow();
        return `<section class="card" id="settings-syslog">
            <div class="net-card-head"><h2>Syslog forwarding</h2>${draftTag(settings.syslog_pending)}</div>
            <p class="form-hint">Send this router&rsquo;s logs to a syslog server, so that they are kept somewhere else and for longer than the router keeps them. UDP sends each message once and does not know whether it arrived; TCP keeps a connection open and sends what waited while the server was away. Messages are RFC 5424, one line each; firewall messages name the rule and what it does. There is no encryption: send them only to a server on a network you trust.</p>
            <div class="settings-sl-rows">${syslogRows()}</div>
            <div class="settings-sl-add"><button type="button" class="cfg-btn" id="sl-add"${locked() || s.servers.length >= 3 ? ' disabled' : ''}>Add a server</button></div>
            <label class="toggle toggle--block settings-send"><input type="checkbox" id="sl-system"${s.system ? ' checked' : ''}${locked() ? ' disabled' : ''}><span><strong>Send the system log</strong><span class="form-hint">What the router&rsquo;s services and the kernel say (the System logs page), without the firewall&rsquo;s lines.</span></span></label>
            <label class="toggle toggle--block settings-send"><input type="checkbox" id="sl-firewall"${s.firewall ? ' checked' : ''}${locked() ? ' disabled' : ''}><span><strong>Send the firewall log</strong><span class="form-hint">The packets that rules with &ldquo;Log matching traffic&rdquo; matched, as they happen, with each rule&rsquo;s name and action.</span></span></label>
            <p class="field-error hidden" id="sl-error"></p>
            ${syslogStatusHtml()}
            <div class="form-actions"><span class="form-hint">Servers in the draft start receiving once you commit. To check that a server is reachable, use <a href="#" data-goto="diagnostics">Diagnostics</a>.</span>
                <button type="button" class="cfg-btn cfg-btn--primary" id="settings-syslog-save" disabled>Save to draft</button></div>
        </section>`;
    }

    function powerCard() {
        if (power.state) {
            const text = power.state === 'reboot'
                ? 'The router is restarting. This page comes back by itself when it is up again, usually after a minute or two.'
                : 'The router is shutting down. To use it again, switch it back on.';
            return `<section class="card" id="settings-power"><div class="net-card-head"><h2>Restart or power off</h2></div><div class="form-message form-message--ok" role="status">${text}</div></section>`;
        }
        const btn = (action, label) => `<button type="button" class="cfg-btn ${action === 'poweroff' ? 'cfg-btn--danger' : ''} settings-power-btn" data-action="${action}"${locked() ? ' disabled' : ''}>${power.arming === action ? 'Click again to confirm' : label}</button>`;
        return `<section class="card" id="settings-power">
            <div class="net-card-head"><h2>Restart or power off</h2></div>
            <p class="form-hint">Every device behind the router loses its connection while it restarts, usually for a minute or two; what you have committed is kept. Powering off needs someone to switch the router back on. This is not possible while a change is being applied or waits for confirmation.</p>
            <div class="form-actions">${btn('reboot', 'Restart the router')}${btn('poweroff', 'Power off')}</div>
        </section>`;
    }

    function resetCard() {
        if (reset.done) {
            return `<section class="card" id="settings-reset"><div class="net-card-head"><h2>Factory reset</h2></div>
                <div class="form-message form-message--ok" role="status">The router is erasing everything it saved and restarting. When it is up again it is a new router: plug a computer into any of its network ports, let it take an address, and open <strong>https://192.168.77.1</strong> to set it up. This page will not come back by itself.</div></section>`;
        }
        const lock = locked() ? ' disabled' : '';
        const form = reset.open ? `
            <div class="form-grid"><div class="form-field"><label for="settings-reset-password">Your password</label>
                <input type="password" id="settings-reset-password" value="${escapeHtml(reset.password)}" autocomplete="current-password" spellcheck="false"${lock}></div></div>
            ${reset.error ? `<p class="field-error" id="settings-reset-error">${escapeHtml(reset.error)}</p>` : ''}
            <div class="form-actions"><button type="button" class="cfg-btn" id="settings-reset-cancel"${lock}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--danger" id="settings-reset-go"${lock}>Erase everything and restart</button></div>`
            : `<div class="form-actions"><button type="button" class="cfg-btn cfg-btn--danger" id="settings-reset-open"${lock}>Factory reset&hellip;</button></div>`;
        return `<section class="card" id="settings-reset">
            <div class="net-card-head"><h2>Factory reset</h2></div>
            <p class="form-hint">Erases everything this router saved: its networks, firewall rules, Wi-Fi, VPNs, certificates, history and every administrator account (the console login too), and starts it again as a new router that must be set up from the beginning. The installed version stays. Nothing is kept and this cannot be undone; to keep your settings, make a backup first. This is not possible while a change is being applied or waits for confirmation.</p>
            ${form}
        </section>`;
    }

    async function doReset() {
        reset.error = '';
        if (!reset.password) { reset.error = 'Enter your password to confirm.'; render(); return; }
        showMessage('');
        busy = true; render();
        const res = await apiFetch('/api/v1/system/factory-reset', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: reset.password }) });
        busy = false;
        reset.password = '';
        if (!res || !res.ok) {
            let e = {};
            try { e = res ? await res.json() : {}; } catch (x) { /* not JSON */ }
            reset.error = (e.errors || []).map(x => x.message).join(' ') || e.error || 'The request could not be made.';
            render();
            return;
        }
        reset.done = true;
        render();
    }

    async function doPower(action) {
        if (power.arming !== action) {
            power.arming = action;
            render();
            setTimeout(() => { if (power.arming === action) { power.arming = null; render(); } }, 5000);
            return;
        }
        power.arming = null;
        showMessage('');
        busy = true; render();
        const res = await apiFetch('/api/v1/system/power', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ action }) });
        busy = false;
        if (!res || !res.ok) {
            let e = {};
            try { e = res ? await res.json() : {}; } catch (x) { /* not JSON */ }
            render();
            showMessage((e.errors || []).map(x => x.message).join(' ') || e.error || 'The request could not be made.', 'error');
            return;
        }
        power.state = action;
        render();
        if (action === 'reboot') {
            // wait until the router has gone and come back, then reload
            let seenDown = false;
            clearInterval(powerTimer);
            powerTimer = setInterval(async () => {
                let up = false;
                try { const r = await fetch('/api/v1/config/status', { credentials: 'same-origin' }); up = r.ok || r.status === 401; } catch (e) { up = false; }
                if (!up) seenDown = true;
                else if (seenDown) { clearInterval(powerTimer); window.location.reload(); }
            }, 3000);
        }
    }

    function render() {
        if (!settings) return;
        const options = option('', 'Choose a country…', settings.country) +
            (settings.countries || []).map(c => option(c.code, `${c.name} (${c.code})`, settings.country)).join('');
        $('settings-body').innerHTML =
            '<h2 class="settings-group">General</h2>' +
            textCard('hostname', 'Host name', 'Name of this router', 'none',
                settings.hostname_pending,
                'The name of this router, such as <code>my-router</code>: letters, digits and hyphens. It is written to the machine itself and shown on the Overview. Empty: the machine keeps the name it has.',
                `<label class="toggle toggle--block settings-send"><input type="checkbox" id="settings-send"${sendNow() ? ' checked' : ''}${locked() ? ' disabled' : ''}><span><strong>Tell my internet provider this name</strong><span class="form-hint">Only a connection that gets its address by DHCP sends it, and only a few providers care. Off by default.</span></span></label>`) +
            `<section class="card">
            <div class="net-card-head"><h2>Country</h2>${draftTag(settings.country_pending)}</div>
            <div class="form-grid"><div class="form-field"><label for="settings-country">Where this router is used</label>
                <select id="settings-country" class="role-select"${locked() ? ' disabled' : ''}>${options}</select></div></div>
            <p class="form-hint">Every country has its own rules for radio: which Wi-Fi channels may be used and how strongly they may transmit. Wi-Fi follows this setting, and cannot be switched on without it.</p>
            ${settings.editable === false ? '<p class="form-hint">A change is being applied or waits for confirmation: settings cannot be edited right now.</p>' : ''}
        </section>` +
            textCard('domain', 'Local domain', 'Domain name of this network', 'none',
                settings.domain_pending,
                'A name such as <code>lan</code> or <code>home.example.com</code>. Set, the devices are told it and can use short names (<code>laptop</code> finds <code>laptop.lan</code>), and the router answers for names inside it by itself. Empty until you set one. The names in it are managed on the <a href="#" data-goto="dns">DNS</a> page.') +
            '<h2 class="settings-group">Time</h2>' +
            timeCard() +
            '<h2 class="settings-group">Logging</h2>' +
            syslogCard() +
            '<h2 class="settings-group">Power</h2>' +
            powerCard() +
            resetCard();
        keys.concat('time', 'syslog').forEach(updateButton);
        startClock();
    }

    // the clock ticks on the page from the time the router reported, in the zone the machine is in
    function startClock() {
        clearInterval(clockTimer);
        clockTimer = null;
        const t = settings && settings.time;
        if (!t || !$('settings-clock')) return;
        const offset = t.now - Date.now();
        const tick = () => {
            const el = $('settings-clock');
            if (!el) { clearInterval(clockTimer); return; }
            const d = new Date(Date.now() + offset);
            let text;
            try {
                text = d.toLocaleString(undefined, { timeZone: t.timezone || 'UTC', dateStyle: 'full', timeStyle: 'medium' });
            } catch (e) {
                text = d.toLocaleString(undefined, { timeZone: 'UTC', dateStyle: 'full', timeStyle: 'medium' }) + ' UTC';
            }
            el.textContent = text + (t.timezone ? ` (${t.timezone})` : '');
        };
        tick();
        clockTimer = setInterval(tick, 1000);
    }

    // ---------------------------------------------------------------- saving

    function clearErrors(...ids) {
        ids.forEach(id => { const e = $(id); if (e) { e.textContent = ''; e.classList.add('hidden'); } });
    }

    // one request for one card; errors on a field show under it, anything else on top of the page
    async function put(url, body, fieldErrors, fail, afterOk) {
        showMessage('');
        clearErrors(...Object.values(fieldErrors));
        busy = true;
        keys.concat('time', 'syslog').forEach(updateButton);
        const res = await apiFetch(url, { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
        busy = false;
        if (!res || !res.ok) {
            let e = {};
            try { e = res ? await res.json() : {}; } catch (x) { /* not JSON */ }
            const rest = [];
            (e.errors || []).forEach(x => {
                const box = fieldErrors[x.field] && $(fieldErrors[x.field]);
                if (box) { box.textContent = (box.textContent ? box.textContent + ' ' : '') + x.message; box.classList.remove('hidden'); } else rest.push(x.message);
            });
            if (!e.errors || rest.length) showMessage(rest.join(' ') || e.error || fail, 'error');
            keys.concat('time', 'syslog').forEach(updateButton);
            return;
        }
        settings = await res.json();
        afterOk();
        render();
        showMessage('Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    function saveText(k) {
        const errBox = { [k]: `settings-${k}-error` };
        if (k === 'hostname') errBox.send_hostname = 'settings-hostname-error';
        return put(TEXT[k].url, TEXT[k].body(), errBox, TEXT[k].fail, () => { typed[k] = null; if (k === 'hostname') typed.send = null; });
    }

    function saveTime() {
        return put('/api/v1/settings/time', { timezone: tzNow(), ntp_enabled: ntpNow(), ntp_servers: serverList(serversNow()) },
            { timezone: 'settings-tz-error', ntp_servers: 'settings-servers-error' }, 'The time settings could not be saved.',
            () => { typed.tz = typed.ntp = typed.servers = null; });
    }

    async function saveSyslog() {
        showMessage('');
        clearErrors('sl-error');
        document.querySelectorAll('.settings-sl-error').forEach(e => { e.textContent = ''; e.classList.add('hidden'); });
        busy = true;
        updateButton('syslog');
        const res = await apiFetch('/api/v1/settings/syslog', { method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(syslogBody()) });
        busy = false;
        if (!res || !res.ok) {
            let e = {};
            try { e = res ? await res.json() : {}; } catch (x) { /* not JSON */ }
            const general = [];
            (e.errors || []).forEach(x => {
                const m = /^syslog\.servers\[(\d+)\]\./.exec(x.field);
                const box = m && $(`sl-error-${m[1]}`);
                if (box) { box.textContent = (box.textContent ? box.textContent + ' ' : '') + x.message; box.classList.remove('hidden'); } else general.push(x.message);
            });
            if (general.length) { const g = $('sl-error'); g.textContent = general.join(' '); g.classList.remove('hidden'); }
            else if (!e.errors) showMessage(e.error || 'The syslog settings could not be saved.', 'error');
            updateButton('syslog');
            return;
        }
        settings = await res.json();
        typed.syslog = null;
        render();
        showMessage('Saved as a draft. Review and commit it from the bar at the top when you are ready.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    async function load() {
        const res = await apiFetch('/api/v1/settings');
        if (!res || !res.ok) { showMessage('Could not load the settings.', 'error'); return; }
        settings = await res.json();
        render();
    }

    async function saveCountry(code) {
        showMessage('');
        busy = true;
        render();
        const res = await apiFetch('/api/v1/settings/country', {
            method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ country: code }),
        });
        busy = false;
        if (!res || !res.ok) {
            let err = {};
            try { err = res ? await res.json() : {}; } catch (e) { /* not JSON */ }
            showMessage((err.errors || []).map(x => x.message).join(' ') || err.error || 'The country could not be saved.', 'error');
            await load();
            return;
        }
        settings = await res.json();
        render();
        showMessage(code ? 'Saved as a draft. Review and commit it from the bar at the top when you are ready.' : 'Country cleared in the draft.', 'ok');
        if (window.ConfigBanner) window.ConfigBanner.refresh();
    }

    // ---------------------------------------------------------------- events

    const keyOf = (el) => keys.find(k => el.id === `settings-${k}`);

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('settings-body');
        body.addEventListener('change', (e) => {
            const id = e.target.id;
            if (id === 'settings-country') saveCountry(e.target.value);
            else if (id === 'settings-send') { typed.send = e.target.checked; updateButton('hostname'); }
            else if (id === 'settings-tz') { typed.tz = e.target.value; clearErrors('settings-tz-error'); updateButton('time'); }
            else if (id === 'sl-system') { syslogEdit().system = e.target.checked; updateButton('syslog'); }
            else if (id === 'sl-firewall') { syslogEdit().firewall = e.target.checked; updateButton('syslog'); }
            else if (e.target.dataset.sl === 'protocol') { syslogEdit().servers[Number(e.target.dataset.i)].protocol = e.target.value; updateButton('syslog'); }
            else if (id === 'settings-ntp') {
                typed.ntp = e.target.checked;
                $('settings-servers-grid').classList.toggle('hidden', !typed.ntp);
                updateButton('time');
            }
        });
        body.addEventListener('input', (e) => {
            if (e.target.id === 'settings-reset-password') { reset.password = e.target.value; reset.error = ''; const err = $('settings-reset-error'); if (err) err.remove(); return; }
            if (e.target.dataset.sl === 'host' || e.target.dataset.sl === 'port') {
                syslogEdit().servers[Number(e.target.dataset.i)][e.target.dataset.sl] = e.target.value;
                clearErrors(`sl-error-${e.target.dataset.i}`, 'sl-error');
                updateButton('syslog');
                return;
            }
            if (e.target.id === 'settings-servers') {
                typed.servers = e.target.value;
                clearErrors('settings-servers-error');
                updateButton('time');
                return;
            }
            const k = keyOf(e.target);
            if (!k) return;
            typed[k] = e.target.value;
            clearErrors(`settings-${k}-error`);
            updateButton(k);
        });
        body.addEventListener('click', (e) => {
            if (e.target.id === 'settings-time-save') { saveTime(); return; }
            if (e.target.id === 'settings-syslog-save') { saveSyslog(); return; }
            if (e.target.classList.contains('settings-power-btn')) { doPower(e.target.dataset.action); return; }
            if (e.target.id === 'settings-reset-open') { reset.open = true; render(); $('settings-reset-password').focus(); return; }
            if (e.target.id === 'settings-reset-cancel') { reset = { open: false, password: '', error: '', done: false }; render(); return; }
            if (e.target.id === 'settings-reset-go') { doReset(); return; }
            if (e.target.id === 'sl-add') {
                const s = syslogEdit();
                if (s.servers.length === 0) { s.system = true; s.firewall = true; }
                s.servers.push({ host: '', port: '514', protocol: 'udp' });
                render();
                $(`sl-host-${s.servers.length - 1}`).focus();
                return;
            }
            if (e.target.classList.contains('settings-sl-remove')) { syslogEdit().servers.splice(Number(e.target.dataset.i), 1); render(); return; }
            const k = keys.find(x => e.target.id === `settings-${x}-save`);
            if (k) saveText(k);
        });
        body.addEventListener('keydown', (e) => {
            if (e.key !== 'Enter' || busy) return;
            if (e.target.id === 'settings-reset-password') { e.preventDefault(); doReset(); return; }
            const k = keyOf(e.target);
            if (k && dirty(k)) { e.preventDefault(); saveText(k); }
            else if (e.target.id === 'settings-servers' && dirty('time')) { e.preventDefault(); saveTime(); }
            else if (e.target.dataset.sl && dirty('syslog')) { e.preventDefault(); saveSyslog(); }
        });
    }

    function init() {
        wire();
        showMessage('');
        settings = null;
        typed = blankTyped();
        load();
        power = { arming: null, state: null };
        return () => { clearInterval(clockTimer); clockTimer = null; };
    }

    return { init };
})();
