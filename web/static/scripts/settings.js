// web/static/scripts/settings.js
// Settings page: choices that apply to the whole router. Today that is the country, which Wi-Fi uses to know
// which channels it may use. Saving only writes a draft.

window.SettingsModule = (function () {
    let settings = null;
    let domainText = null;       // what is typed in the domain box (null: what the router has)
    let busy = false;
    let wired = false;

    const $ = (id) => document.getElementById(id);

    function showMessage(text, kind) {
        const box = $('settings-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    const option = (value, label, current) =>
        `<option value="${escapeHtml(value)}"${value === current ? ' selected' : ''}>${escapeHtml(label)}</option>`;

    function render() {
        if (!settings) return;
        const lock = busy || settings.editable === false;
        const options = option('', 'Choose a country…', settings.country) +
            (settings.countries || []).map(c => option(c.code, `${c.name} (${c.code})`, settings.country)).join('');
        $('settings-body').innerHTML = `<section class="card">
            <div class="net-card-head"><h2>Country</h2>${settings.country_pending ? '<span class="tag tag-draft">Draft</span>' : ''}</div>
            <div class="form-grid"><div class="form-field"><label for="settings-country">Where this router is used</label>
                <select id="settings-country" class="role-select"${lock ? ' disabled' : ''}>${options}</select></div></div>
            <p class="form-hint">Every country has its own rules for radio: which Wi-Fi channels may be used and how strongly they may transmit. Wi-Fi follows this setting, and cannot be switched on without it.</p>
            ${settings.editable === false ? '<p class="form-hint">A change is being applied or waits for confirmation: settings cannot be edited right now.</p>' : ''}
        </section>
        <section class="card">
            <div class="net-card-head"><h2>Local domain</h2>${settings.domain_pending ? '<span class="tag tag-draft">Draft</span>' : ''}</div>
            <div class="form-grid"><div class="form-field"><label for="settings-domain">Domain name of this network</label>
                <input type="text" id="settings-domain" value="${escapeHtml(domainText !== null ? domainText : settings.domain)}" placeholder="none" autocomplete="off" spellcheck="false" autocapitalize="off"${lock ? ' disabled' : ''}>
                <p class="field-error hidden" id="settings-domain-error"></p></div></div>
            <p class="form-hint">A name such as <code>lan</code> or <code>home.example.com</code>. Set, the devices are told it and can use short names (<code>laptop</code> finds <code>laptop.lan</code>), and the router answers for names inside it by itself. Empty until you set one. The names in it are managed on the <a href="#" data-goto="dns">DNS</a> page.</p>
            <div class="form-actions">
                <button type="button" class="cfg-btn cfg-btn--primary" id="settings-domain-save" disabled>Save to draft</button>
            </div>
        </section>`;
        updateDomainButton();
    }

    function domainDirty() {
        return settings && domainText !== null && domainText.trim().toLowerCase() !== (settings.domain || '');
    }

    function updateDomainButton() {
        const b = $('settings-domain-save');
        if (b) b.disabled = busy || settings.editable === false || !domainDirty();
    }

    async function saveDomain() {
        showMessage('');
        const err = $('settings-domain-error');
        if (err) { err.textContent = ''; err.classList.add('hidden'); }
        busy = true;
        updateDomainButton();
        const res = await apiFetch('/api/v1/settings/domain', {
            method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ domain: domainText.trim() }),
        });
        busy = false;
        if (!res || !res.ok) {
            let e = {};
            try { e = res ? await res.json() : {}; } catch (x) { /* not JSON */ }
            const msg = (e.errors || []).map(x => x.message).join(' ') || e.error || 'The domain could not be saved.';
            if (err && (e.errors || []).some(x => x.field === 'domain')) { err.textContent = msg; err.classList.remove('hidden'); }
            else showMessage(msg, 'error');
            updateDomainButton();
            return;
        }
        settings = await res.json();
        domainText = null;
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

    function wire() {
        if (wired) return;
        wired = true;
        $('settings-body').addEventListener('change', (e) => {
            if (e.target.id === 'settings-country') saveCountry(e.target.value);
        });
        $('settings-body').addEventListener('input', (e) => {
            if (e.target.id !== 'settings-domain') return;
            domainText = e.target.value;
            const err = $('settings-domain-error');
            if (err) { err.textContent = ''; err.classList.add('hidden'); }
            updateDomainButton();
        });
        $('settings-body').addEventListener('click', (e) => {
            if (e.target.id === 'settings-domain-save') saveDomain();
        });
        $('settings-body').addEventListener('keydown', (e) => {
            if (e.key === 'Enter' && e.target.id === 'settings-domain' && domainDirty() && !busy) { e.preventDefault(); saveDomain(); }
        });
    }

    function init() {
        wire();
        showMessage('');
        settings = null;
        domainText = null;
        load();
        return () => {};
    }

    return { init };
})();
