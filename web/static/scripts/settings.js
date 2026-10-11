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

    const $ = (id) => document.getElementById(id);

    function blankTyped() { return { hostname: null, domain: null, send: null, tz: null, ntp: null, servers: null }; }

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
            timeCard();
        keys.concat('time').forEach(updateButton);
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
        keys.concat('time').forEach(updateButton);
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
            keys.concat('time').forEach(updateButton);
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
            else if (id === 'settings-ntp') {
                typed.ntp = e.target.checked;
                $('settings-servers-grid').classList.toggle('hidden', !typed.ntp);
                updateButton('time');
            }
        });
        body.addEventListener('input', (e) => {
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
            const k = keys.find(x => e.target.id === `settings-${x}-save`);
            if (k) saveText(k);
        });
        body.addEventListener('keydown', (e) => {
            if (e.key !== 'Enter' || busy) return;
            const k = keyOf(e.target);
            if (k && dirty(k)) { e.preventDefault(); saveText(k); }
            else if (e.target.id === 'settings-servers' && dirty('time')) { e.preventDefault(); saveTime(); }
        });
    }

    function init() {
        wire();
        showMessage('');
        settings = null;
        typed = blankTyped();
        load();
        return () => { clearInterval(clockTimer); clockTimer = null; };
    }

    return { init };
})();
