// web/static/scripts/settings.js
// Settings page: choices that apply to the whole router: its host name, the country (which Wi-Fi uses to know which
// channels it may use) and the local domain. Saving only writes a draft.

window.SettingsModule = (function () {
    let settings = null;
    let typed = { hostname: null, domain: null, send: null };   // what is typed in each text box, and the sending box (null: what the router has)
    let busy = false;
    let wired = false;

    const $ = (id) => document.getElementById(id);

    // the two settings that are a line of text: where they are saved, and how the router keeps them
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

    function showMessage(text, kind) {
        const box = $('settings-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    const option = (value, label, current) =>
        `<option value="${escapeHtml(value)}"${value === current ? ' selected' : ''}>${escapeHtml(label)}</option>`;

    const draftTag = (on) => (on ? '<span class="tag tag-draft">Draft</span>' : '');
    const shown = (k) => (typed[k] !== null ? typed[k] : settings[k] || '');
    const sendNow = () => (typed.send !== null ? typed.send : !!settings.send_hostname);

    function textCard(k, title, label, placeholder, pending, hint, extra) {
        const lock = busy || settings.editable === false;
        return `<section class="card">
            <div class="net-card-head"><h2>${title}</h2>${draftTag(pending)}</div>
            <div class="form-grid"><div class="form-field"><label for="settings-${k}">${label}</label>
                <input type="text" id="settings-${k}" value="${escapeHtml(shown(k))}" placeholder="${placeholder}" autocomplete="off" spellcheck="false" autocapitalize="off"${lock ? ' disabled' : ''}>
                <p class="field-error hidden" id="settings-${k}-error"></p></div></div>
            <p class="form-hint">${hint}</p>
            ${extra || ''}
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--primary" id="settings-${k}-save" disabled>Save to draft</button>
            </div>
        </section>`;
    }

    function render() {
        if (!settings) return;
        const lock = busy || settings.editable === false;
        const options = option('', 'Choose a country…', settings.country) +
            (settings.countries || []).map(c => option(c.code, `${c.name} (${c.code})`, settings.country)).join('');
        $('settings-body').innerHTML =
            textCard('hostname', 'Host name', 'Name of this router', 'none',
                settings.hostname_pending,
                'The name of this router, such as <code>my-router</code>: letters, digits and hyphens. It is written to the machine itself and shown on the Overview. Empty: the machine keeps the name it has.',
                `<label class="toggle toggle--block settings-send"><input type="checkbox" id="settings-send"${sendNow() ? ' checked' : ''}${busy || settings.editable === false ? ' disabled' : ''}><span><strong>Tell my internet provider this name</strong><span class="form-hint">Only a connection that gets its address by DHCP sends it, and only a few providers care. Off by default.</span></span></label>`) +
            `<section class="card">
            <div class="net-card-head"><h2>Country</h2>${draftTag(settings.country_pending)}</div>
            <div class="form-grid"><div class="form-field"><label for="settings-country">Where this router is used</label>
                <select id="settings-country" class="role-select"${lock ? ' disabled' : ''}>${options}</select></div></div>
            <p class="form-hint">Every country has its own rules for radio: which Wi-Fi channels may be used and how strongly they may transmit. Wi-Fi follows this setting, and cannot be switched on without it.</p>
            ${settings.editable === false ? '<p class="form-hint">A change is being applied or waits for confirmation: settings cannot be edited right now.</p>' : ''}
        </section>` +
            textCard('domain', 'Local domain', 'Domain name of this network', 'none',
                settings.domain_pending,
                'A name such as <code>lan</code> or <code>home.example.com</code>. Set, the devices are told it and can use short names (<code>laptop</code> finds <code>laptop.lan</code>), and the router answers for names inside it by itself. Empty until you set one. The names in it are managed on the <a href="#" data-goto="dns">DNS</a> page.');
        keys.forEach(updateButton);
    }

    function dirty(k) {
        if (!settings) return false;
        const text = typed[k] !== null && TEXT[k].tidy(typed[k]) !== (settings[k] || '');
        return text || (k === 'hostname' && typed.send !== null && typed.send !== !!settings.send_hostname);
    }

    function updateButton(k) {
        const b = $(`settings-${k}-save`);
        if (b) b.disabled = busy || settings.editable === false || !dirty(k);
    }

    async function saveText(k) {
        showMessage('');
        const err = $(`settings-${k}-error`);
        if (err) { err.textContent = ''; err.classList.add('hidden'); }
        busy = true;
        updateButton(k);
        const res = await apiFetch(TEXT[k].url, {
            method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(TEXT[k].body()),
        });
        busy = false;
        if (!res || !res.ok) {
            let e = {};
            try { e = res ? await res.json() : {}; } catch (x) { /* not JSON */ }
            const msg = (e.errors || []).map(x => x.message).join(' ') || e.error || TEXT[k].fail;
            if (err && (e.errors || []).some(x => x.field === k)) { err.textContent = msg; err.classList.remove('hidden'); }
            else showMessage(msg, 'error');
            updateButton(k);
            return;
        }
        settings = await res.json();
        typed[k] = null;
        if (k === 'hostname') typed.send = null;
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

    const keyOf = (el) => keys.find(k => el.id === `settings-${k}`);

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('settings-body');
        body.addEventListener('change', (e) => {
            if (e.target.id === 'settings-country') saveCountry(e.target.value);
            else if (e.target.id === 'settings-send') { typed.send = e.target.checked; updateButton('hostname'); }
        });
        body.addEventListener('input', (e) => {
            const k = keyOf(e.target);
            if (!k) return;
            typed[k] = e.target.value;
            const err = $(`settings-${k}-error`);
            if (err) { err.textContent = ''; err.classList.add('hidden'); }
            updateButton(k);
        });
        body.addEventListener('click', (e) => {
            const k = keys.find(x => e.target.id === `settings-${x}-save`);
            if (k) saveText(k);
        });
        body.addEventListener('keydown', (e) => {
            const k = e.key === 'Enter' && keyOf(e.target);
            if (k && dirty(k) && !busy) { e.preventDefault(); saveText(k); }
        });
    }

    function init() {
        wire();
        showMessage('');
        settings = null;
        typed = { hostname: null, domain: null, send: null };
        load();
        return () => {};
    }

    return { init };
})();
