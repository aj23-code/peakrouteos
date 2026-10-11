// web/static/scripts/settings.js
// Settings page: choices that apply to the whole router. Today that is the country, which Wi-Fi uses to know
// which channels it may use. Saving only writes a draft.

window.SettingsModule = (function () {
    let settings = null;
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
        </section>`;
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
    }

    function init() {
        wire();
        showMessage('');
        settings = null;
        load();
        return () => {};
    }

    return { init };
})();
