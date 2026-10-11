// web/static/scripts/admins.js
// Administrators page: who may sign in to the web interface, their role and password, and how long a session may sit
// idle. Unlike the other pages these changes take effect at once: they are not part of the draft that is committed.
// docs/administrators.md describes the rules.

window.AdminsModule = (function () {
    let data = null;             // GET /api/v1/admins
    let own = null;              // the change-my-password form: { current, password, confirm, errors, message, ok }
    let reset = null;            // the set-a-password form of another account: { name, password, confirm, errors, message }
    let adding = null;           // the new-account form: { username, role, password, confirm, errors, message }
    let timeoutText = null;      // what is typed in the timeout box (null: what the router has)
    let timeoutError = null;
    let confirmDelete = null;    // the account whose delete button was pressed once
    let busy = false;
    let wired = false;

    const $ = (id) => document.getElementById(id);
    const ROLES = { admin: 'Administrator', readonly: 'Read-only' };

    function showMessage(text, kind) {
        const box = $('admins-message');
        box.textContent = text || '';
        box.classList.toggle('hidden', !text);
        box.classList.toggle('form-message--ok', kind === 'ok');
    }

    async function errorOf(res, fallback) {
        if (!res) return { general: 'The router did not answer. Try again.', fields: {} };
        let err = {};
        try { err = await res.json(); } catch (e) { /* not JSON */ }
        const fields = {};
        (err.errors || []).forEach(x => { fields[x.field] = x.message; });
        return { general: err.error || fallback, fields };
    }

    const fieldError = (e) => (e ? `<p class="field-error">${escapeHtml(e)}</p>` : '');
    const blankOwn = () => ({ current: '', password: '', confirm: '', errors: {}, message: null });

    function passwordField(label, id, key, form, autocomplete, hint) {
        const lock = busy;
        return `<div class="form-field"><label for="${id}">${label}</label>
            <input type="password" id="${id}" data-form="${key}" data-key="${id.split('-').pop()}" value="${escapeHtml(form[id.split('-').pop()] || '')}" autocomplete="${autocomplete}" spellcheck="false"${form.errors[id.split('-').pop()] ? ' aria-invalid="true"' : ''}${lock ? ' disabled' : ''}>
            ${hint ? `<p class="form-hint">${hint}</p>` : ''}${fieldError(form.errors[id.split('-').pop()])}</div>`;
    }

    // ---------------------------------------------------------------- the cards

    function ownCard() {
        const f = own;
        const complete = f.current && f.password && f.confirm;
        return `<section class="card" id="admins-own">
            <div class="net-card-head"><h2>Your account</h2></div>
            <p class="form-hint">You are signed in as <strong>${escapeHtml(data.you)}</strong> (${ROLES[data.role] || data.role}).${data.can_manage ? '' : ' A read-only account can look at every page but cannot change anything except its own password.'}</p>
            <h3 class="form-section">Change your password</h3>
            <div class="form-grid">
                ${passwordField('Current password', 'own-current', 'own', f, 'current-password')}
                ${passwordField('New password', 'own-password', 'own', f, 'new-password', `At least ${data.min_password} characters (${data.max_password_bytes} bytes at most).`)}
                ${passwordField('New password again', 'own-confirm', 'own', f, 'new-password')}
            </div>
            ${f.message ? `<div class="form-message${f.ok ? ' form-message--ok' : ''}" role="alert">${escapeHtml(f.message)}</div>` : ''}
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--primary" id="own-save"${busy || !complete ? ' disabled' : ''}>Change password</button></div>
        </section>`;
    }

    function accountRow(u) {
        const key = `del:${u.username}`;
        const resetting = reset && reset.name === u.username;
        const name = escapeHtml(u.username);
        const lock = busy || !!adding;
        const role = u.you
            ? `<span class="tag ${u.role === 'admin' ? 'tag-ok' : ''}">${ROLES[u.role]}</span>`
            : `<select class="role-select admins-role" data-name="${name}" aria-label="Role of ${name}"${lock ? ' disabled' : ''}>
                <option value="admin"${u.role === 'admin' ? ' selected' : ''}>Administrator</option>
                <option value="readonly"${u.role === 'readonly' ? ' selected' : ''}>Read-only</option></select>`;
        const sessions = u.sessions ? `${u.sessions} signed in` : 'not signed in';
        const actions = u.you
            ? `<button type="button" class="cfg-btn cfg-btn--ghost admins-signout" data-name="${name}"${lock || !u.sessions || u.sessions < 2 ? ' disabled' : ''} title="Sign out every other browser or device that is signed in as you">Sign out other sessions</button>`
            : `<button type="button" class="cfg-btn cfg-btn--ghost admins-reset" data-name="${name}"${lock ? ' disabled' : ''}>Set password</button>
               <button type="button" class="cfg-btn cfg-btn--ghost admins-signout" data-name="${name}"${lock || !u.sessions ? ' disabled' : ''}>Sign out</button>
               <button type="button" class="cfg-btn cfg-btn--danger admins-delete" data-name="${name}"${lock ? ' disabled' : ''}>${confirmDelete === key ? 'Click again' : 'Delete'}</button>`;
        const resetForm = resetting ? `<div class="admins-reset-form">
                <div class="form-grid">
                    ${passwordField(`New password for ${name}`, 'reset-password', 'reset', reset, 'new-password')}
                    ${passwordField('New password again', 'reset-confirm', 'reset', reset, 'new-password')}
                </div>
                <p class="form-hint">${name} is signed out everywhere and signs in again with the new password.</p>
                ${reset.message ? `<div class="form-message" role="alert">${escapeHtml(reset.message)}</div>` : ''}
                <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--ghost" id="reset-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                    <button type="button" class="cfg-btn cfg-btn--primary" id="reset-save"${busy || !reset.password || !reset.confirm ? ' disabled' : ''}>Set password</button></div>
            </div>` : '';
        return `<div class="fw-row admins-row"><div class="fw-main"><div class="fw-name"><strong>${name}</strong>${u.you ? ' <span class="tag">you</span>' : ''}</div>
                <div class="form-hint">${sessions}</div></div>
            <div class="admins-role-cell">${role}</div>
            <div class="fw-actions">${actions}</div></div>${resetForm ? `<div class="admins-reset-row">${resetForm}</div>` : ''}`;
    }

    function accountsCard() {
        const lock = busy || !!adding;
        return `<section class="card" id="admins-list">
            <div class="net-card-head"><h2>Accounts</h2><span class="tag">${data.users.length}</span>
                <button type="button" class="cfg-btn" id="admins-add" style="margin-left:auto"${lock || data.users.length >= data.max_accounts ? ' disabled' : ''}>Add account</button></div>
            <p class="form-hint"><strong>Administrators</strong> can change everything, including these accounts. <strong>Read-only</strong> accounts can look at every page and change nothing. There is always at least one administrator, and you cannot remove or change the account you are signed in with.</p>
            <div class="fw-list">${data.users.map(accountRow).join('')}</div>
        </section>`;
    }

    function addCard() {
        const f = adding;
        const complete = f.username.trim() && f.password && f.confirm;
        return `<section class="card fw-editor" id="admins-editor">
            <div class="net-card-head"><h2>New account</h2></div>
            <div class="form-grid">
                <div class="form-field"><label for="add-username">User name</label>
                    <input type="text" id="add-username" data-form="add" data-key="username" value="${escapeHtml(f.username)}" autocomplete="off" spellcheck="false" autocapitalize="off"${f.errors.username ? ' aria-invalid="true"' : ''}${busy ? ' disabled' : ''}>
                    <p class="form-hint">3 to 32 characters: letters, digits, dot, underscore and hyphen.</p>${fieldError(f.errors.username)}</div>
                <div class="form-field"><label for="add-role">Role</label>
                    <select id="add-role" class="role-select" data-form="add" data-key="role"${busy ? ' disabled' : ''}>
                        <option value="readonly"${f.role === 'readonly' ? ' selected' : ''}>Read-only</option>
                        <option value="admin"${f.role === 'admin' ? ' selected' : ''}>Administrator</option></select>${fieldError(f.errors.role)}</div>
                ${passwordField('Password', 'add-password', 'add', f, 'new-password', `At least ${data.min_password} characters.`)}
                ${passwordField('Password again', 'add-confirm', 'add', f, 'new-password')}
            </div>
            ${f.message ? `<div class="form-message" role="alert">${escapeHtml(f.message)}</div>` : ''}
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--ghost" id="add-cancel"${busy ? ' disabled' : ''}>Cancel</button>
                <button type="button" class="cfg-btn cfg-btn--primary" id="add-save"${busy || !complete ? ' disabled' : ''}>Add account</button></div>
        </section>`;
    }

    const timeoutValue = () => (timeoutText !== null ? timeoutText : String(data.session_timeout_minutes));
    const timeoutDirty = () => timeoutText !== null && timeoutText.trim() !== String(data.session_timeout_minutes);

    function timeoutCard() {
        return `<section class="card" id="admins-timeout">
            <div class="net-card-head"><h2>Session timeout</h2></div>
            <div class="form-grid"><div class="form-field"><label for="timeout-minutes">Sign out after this many idle minutes</label>
                <input type="text" id="timeout-minutes" value="${escapeHtml(timeoutValue())}" inputmode="numeric" autocomplete="off" spellcheck="false"${timeoutError ? ' aria-invalid="true"' : ''}${busy ? ' disabled' : ''}>
                ${fieldError(timeoutError)}</div></div>
            <p class="form-hint">A session ends after this long without any activity, from ${data.timeout_min} minute to 24 hours (${data.timeout_max} minutes); the usual is ${data.timeout_default}. It applies to everyone, and from their next click on to people who are signed in now.</p>
            <div class="form-actions"><button type="button" class="cfg-btn cfg-btn--primary" id="timeout-save"${busy || !timeoutDirty() ? ' disabled' : ''}>Save</button></div>
        </section>`;
    }

    function render() {
        if (!data) return;
        $('admins-body').innerHTML = ownCard() + (data.can_manage ? accountsCard() + (adding ? addCard() : '') + timeoutCard() : '');
    }

    // ---------------------------------------------------------------- calls

    async function load() {
        const res = await apiFetch('/api/v1/admins');
        if (!res || !res.ok) { showMessage('Could not load the accounts.', 'error'); return; }
        data = await res.json();
        render();
    }

    async function send(method, url, body, keepOwn) {
        if (!keepOwn && own && !own.ok) { own.message = null; own.errors = {}; } // a complaint about the password form is stale once something else is done
        busy = true; render();
        const res = await apiFetch(url, { method, headers: { 'Content-Type': 'application/json' }, body: body === undefined ? undefined : JSON.stringify(body) });
        busy = false;
        return res;
    }

    async function saveOwn() {
        own.errors = {}; own.message = null; own.ok = false;
        const res = await send('POST', '/api/v1/me/password', { current: own.current, password: own.password, confirm: own.confirm }, true);
        if (!res || !res.ok) {
            const e = await errorOf(res, 'The password could not be changed.');
            own.errors = e.fields;
            own.message = Object.keys(e.fields).length ? 'Some entries need attention.' : e.general;
            own.password = ''; own.confirm = ''; if (e.fields.current) own.current = '';
            render();
            return;
        }
        const out = await res.json();
        own = blankOwn();
        own.ok = true;
        own.message = out.ended ? `Your password is changed. ${out.ended} other ${out.ended === 1 ? 'session was' : 'sessions were'} signed out.` : 'Your password is changed.';
        await load();
    }

    async function saveReset() {
        const f = reset;
        f.errors = {}; f.message = null;
        const res = await send('POST', `/api/v1/admins/${encodeURIComponent(f.name)}/password`, { password: f.password, confirm: f.confirm });
        if (!res || !res.ok) {
            const e = await errorOf(res, 'The password could not be set.');
            f.errors = e.fields; f.password = ''; f.confirm = '';
            f.message = Object.keys(e.fields).length ? 'Some entries need attention.' : e.general;
            render();
            return;
        }
        const name = f.name;
        reset = null;
        data = await res.json();
        render();
        showMessage(`The password of ${name} is set, and ${name} is signed out everywhere.`, 'ok');
    }

    async function saveAdd() {
        const f = adding;
        f.errors = {}; f.message = null;
        const res = await send('POST', '/api/v1/admins', { username: f.username.trim(), role: f.role, password: f.password, confirm: f.confirm });
        if (!res || !res.ok) {
            const e = await errorOf(res, 'The account could not be added.');
            f.errors = e.fields; f.password = ''; f.confirm = '';
            f.message = Object.keys(e.fields).length ? 'Some entries need attention.' : e.general;
            render();
            return;
        }
        const name = f.username.trim();
        adding = null;
        data = await res.json();
        render();
        showMessage(`The account ${name} is added.`, 'ok');
    }

    async function simple(method, url, body, done) {
        showMessage('');
        const res = await send(method, url, body);
        if (!res || !res.ok) {
            showMessage((await errorOf(res, 'The change could not be made.')).general, 'error');
            await load();
            return;
        }
        data = await res.json();
        render();
        showMessage(done, 'ok');
    }

    async function remove(name) {
        const key = `del:${name}`;
        if (confirmDelete !== key) {
            confirmDelete = key; render();
            setTimeout(() => { if (confirmDelete === key) { confirmDelete = null; render(); } }, 4000);
            return;
        }
        confirmDelete = null;
        await simple('DELETE', `/api/v1/admins/${encodeURIComponent(name)}`, undefined, `The account ${name} is removed and signed out.`);
    }

    async function saveTimeout() {
        timeoutError = null;
        const minutes = Number(timeoutText.trim());
        if (!Number.isInteger(minutes) || minutes < data.timeout_min || minutes > data.timeout_max) {
            timeoutError = `Choose a whole number of minutes from ${data.timeout_min} to ${data.timeout_max}.`;
            render();
            return;
        }
        const res = await send('PUT', '/api/v1/session/timeout', { minutes });
        if (!res || !res.ok) {
            timeoutError = (await errorOf(res, 'The setting could not be saved.')).general;
            render();
            return;
        }
        data = await res.json();
        timeoutText = null;
        render();
        showMessage(`Sessions now end after ${minutes} idle ${minutes === 1 ? 'minute' : 'minutes'}.`, 'ok');
    }

    // ---------------------------------------------------------------- events

    function refreshButtons() {
        // typing must not rebuild the page (the focus would be lost): only the buttons follow
        const set = (id, off) => { const b = $(id); if (b) b.disabled = busy || off; };
        if (own) set('own-save', !(own.current && own.password && own.confirm));
        if (reset) set('reset-save', !(reset.password && reset.confirm));
        if (adding) set('add-save', !(adding.username.trim() && adding.password && adding.confirm));
        if (data && data.can_manage) set('timeout-save', !timeoutDirty());
    }

    const forms = () => ({ own, reset, add: adding });

    function wire() {
        if (wired) return;
        wired = true;
        const body = $('admins-body');
        body.addEventListener('click', (e) => {
            const btn = e.target.closest('button');
            if (!btn) return;
            const name = btn.dataset.name;
            if (btn.id === 'own-save') saveOwn();
            else if (btn.id === 'admins-add') { reset = null; adding = { username: '', role: 'readonly', password: '', confirm: '', errors: {}, message: null }; render(); $('add-username').focus(); }
            else if (btn.id === 'add-cancel') { adding = null; render(); }
            else if (btn.id === 'add-save') saveAdd();
            else if (btn.id === 'reset-cancel') { reset = null; render(); }
            else if (btn.id === 'reset-save') saveReset();
            else if (btn.id === 'timeout-save') saveTimeout();
            else if (btn.classList.contains('admins-reset')) { reset = { name, password: '', confirm: '', errors: {}, message: null }; render(); $('reset-password').focus(); }
            else if (btn.classList.contains('admins-delete')) remove(name);
            else if (btn.classList.contains('admins-signout')) simple('DELETE', `/api/v1/admins/${encodeURIComponent(name)}/sessions`, undefined, name === data.you ? 'Your other sessions are signed out.' : `${name} is signed out everywhere.`);
        });
        body.addEventListener('change', (e) => {
            const el = e.target;
            if (el.classList.contains('admins-role')) simple('PUT', `/api/v1/admins/${encodeURIComponent(el.dataset.name)}`, { role: el.value }, `${el.dataset.name} is now ${ROLES[el.value].toLowerCase()}.`);
            else if (el.dataset.form === 'add' && el.dataset.key === 'role' && adding) adding.role = el.value;
        });
        body.addEventListener('input', (e) => {
            const el = e.target;
            if (el.id === 'timeout-minutes') { timeoutText = el.value; timeoutError = null; el.removeAttribute('aria-invalid'); refreshButtons(); return; }
            const form = forms()[el.dataset.form];
            if (!form || !el.dataset.key || el.tagName === 'SELECT') return;
            form[el.dataset.key] = el.value;
            delete form.errors[el.dataset.key];
            el.removeAttribute('aria-invalid');
            el.closest('.form-field')?.querySelector('.field-error')?.remove();
            refreshButtons();
        });
        body.addEventListener('keydown', (e) => {
            if (e.key !== 'Enter' || busy || e.target.tagName !== 'INPUT') return;
            const id = e.target.id;
            const go = { 'own-current': 'own-save', 'own-password': 'own-save', 'own-confirm': 'own-save', 'reset-password': 'reset-save', 'reset-confirm': 'reset-save',
                'add-username': 'add-save', 'add-password': 'add-save', 'add-confirm': 'add-save', 'timeout-minutes': 'timeout-save' }[id];
            const btn = go && $(go);
            if (btn && !btn.disabled) { e.preventDefault(); btn.click(); }
        });
    }

    function init() {
        wire();
        showMessage('');
        data = null; own = blankOwn(); reset = null; adding = null; timeoutText = null; timeoutError = null; confirmDelete = null; busy = false;
        load();
        return () => { own = null; reset = null; adding = null; };
    }

    return { init };
})();
