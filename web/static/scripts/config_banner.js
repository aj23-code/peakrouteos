// web/static/scripts/config_banner.js
//
// The "uncommitted changes" banner shown at the top of every view, the review dialog behind it,
// and the 60-second confirm countdown. All state lives on the server (GET /api/v1/config/status);
// this file only shows it and sends the user's choices (commit, discard, confirm, revert).

window.ConfigBanner = (function () {
    const POLL_IDLE_MS = 4000;
    const POLL_BUSY_MS = 1000;
    const POLL_LOST_MS = 2000;
    const SUCCESS_SHOWN_MS = 8000;   // how long good news ("the change was confirmed") stays up

    const SERVICE_TEXT = {
        hostname: 'Set the host name',
        sysctl: 'Apply kernel settings (IP forwarding)',
        networking: 'Restart networking (a brief interruption)',
        routes: 'Apply the static routes',
        wifi: 'Restart Wi-Fi (devices reconnect)',
        nftables: 'Reload the firewall rules',
        timezone: 'Set the time zone',
        ntp: 'Set up the time servers',
        wireguard: 'Set up the WireGuard tunnels',
        openvpn: 'Set up the OpenVPN tunnels',
        dnsmasq: 'Restart the DHCP and DNS server',
    };
    const FILE_KIND_TEXT = { create: 'Create', update: 'Update', remove: 'Remove' };

    let status = null;
    let deadlineMs = null;       // when the confirm window ends, by this browser's clock
    let lost = false;            // the router cannot be reached right now
    let pollTimer = null;
    let reviewing = null;        // the review currently shown in the dialog

    // ---- small helpers ------------------------------------------------------------------

    const esc = (v) => escapeHtml(v);

    // sessionStorage can be missing or blocked: never let it break the banner
    function remember(key, value) {
        try { sessionStorage.setItem('peakroute.' + key, value); } catch (e) { /* optional */ }
    }
    function recall(key) {
        try { return sessionStorage.getItem('peakroute.' + key); } catch (e) { return null; }
    }
    function forget(key) {
        try { sessionStorage.removeItem('peakroute.' + key); } catch (e) { /* optional */ }
    }

    // ---- what to show (pure, so it can be checked on its own) ----------------------------

    /**
     * Decide what the banner shows for a status.
     *   ctx.lost         the router cannot be reached
     *   ctx.newUrl       where to reconnect after a LAN address change (or null)
     *   ctx.dismissedAt  the `last.at` of an outcome the user already dismissed
     *   ctx.seenAt       the `last.at` of the last good-news outcome already shown
     *   ctx.seenForMs    how long ago that good news was first shown
     * Returns { primary, outcome }; either may be null.
     */
    function describe(st, ctx) {
        ctx = ctx || {};
        const result = { primary: null, outcome: null };

        if (ctx.lost) {
            result.primary = { kind: 'lost', newUrl: ctx.newUrl || null };
            return result;
        }
        if (!st) return result;

        if (st.state === 'awaiting_confirm') {
            result.primary = { kind: 'confirm', secondsLeft: st.confirm ? st.confirm.seconds_left : 0 };
        } else if (st.state === 'applying') {
            result.primary = { kind: 'applying' };
        } else if (st.has_changes) {
            result.primary = { kind: 'changes', count: st.change_count };
        }

        const last = st.last;
        if (last && last.at && last.at !== ctx.dismissedAt) {
            const bad = last.kind === 'failed' || last.kind === 'reverted';
            // Problems stay until dismissed; good news is shown for a few seconds, once
            const firstTime = last.at !== ctx.seenAt;
            if (bad || firstTime || (ctx.seenForMs || 0) < SUCCESS_SHOWN_MS) {
                result.outcome = { kind: bad ? 'bad' : 'ok', outcome: last };
            }
        }
        return result;
    }

    // ---- rendering ----------------------------------------------------------------------

    function plural(n, one, many) { return n + ' ' + (n === 1 ? one : many); }

    function primaryHtml(p) {
        switch (p.kind) {
            case 'changes':
                return `<div class="config-bar config-bar--changes" role="status">
                    <span class="config-bar-text"><strong>${esc(plural(p.count, 'uncommitted change', 'uncommitted changes'))}.</strong>
                    They are saved as a draft and are not active yet.</span>
                    <span class="config-bar-actions">
                        <button type="button" class="cfg-btn cfg-btn--primary" data-config-action="review">Review &amp; commit</button>
                        <button type="button" class="cfg-btn cfg-btn--ghost" data-config-action="discard">Discard</button>
                    </span></div>`;
            case 'applying':
                return `<div class="config-bar config-bar--applying" role="status">
                    <span class="config-bar-text"><strong>Applying changes&hellip;</strong>
                    The router may be unreachable for a moment.</span></div>`;
            case 'confirm':
                return `<div class="config-bar config-bar--confirm" role="alert">
                    <span class="config-bar-text"><strong>Confirm this change.</strong>
                    If you do nothing it is reverted in <strong id="config-countdown">${esc(p.secondsLeft)}</strong>&nbsp;seconds.</span>
                    <span class="config-bar-actions">
                        <button type="button" class="cfg-btn cfg-btn--primary" data-config-action="confirm">Confirm</button>
                        <button type="button" class="cfg-btn cfg-btn--danger" data-config-action="revert">Revert now</button>
                    </span></div>`;
            case 'lost': {
                const where = p.newUrl
                    ? `If you changed the router's LAN address, open <strong>${esc(p.newUrl)}</strong>, log in, and confirm there before the time runs out. Otherwise the change reverts by itself.`
                    : 'Retrying&hellip;';
                return `<div class="config-bar config-bar--lost" role="alert">
                    <span class="config-bar-text"><strong>Lost contact with the router.</strong> ${where}</span></div>`;
            }
        }
        return '';
    }

    function stepsHtml(o) {
        const bad = (o.steps || []).filter((s) => !s.ok);
        const problems = o.problems || [];
        if (!bad.length && !problems.length) return '';
        const items = bad.map((s) => `<li><code>${esc(s.name)}</code>${s.output ? ': ' + esc(s.output) : s.error ? ': ' + esc(s.error) : ''}</li>`)
            .concat(problems.map((p) => `<li>${esc(p)}</li>`));
        return `<ul class="config-bar-details">${items.join('')}</ul>`;
    }

    function outcomeHtml(o) {
        const cls = o.kind === 'bad' ? 'config-bar--bad' : 'config-bar--ok';
        return `<div class="config-bar ${cls}" role="status">
            <span class="config-bar-text">${esc(o.outcome.message)}${stepsHtml(o.outcome)}</span>
            <span class="config-bar-actions">
                <button type="button" class="cfg-btn cfg-btn--ghost" data-config-action="dismiss" data-at="${esc(o.outcome.at)}">Dismiss</button>
            </span></div>`;
    }

    function render() {
        const host = document.getElementById('config-banner');
        if (!host) return;

        const seenAt = recall('seenAt');
        const shown = describe(status, {
            lost,
            newUrl: recall('pendingUrl'),
            dismissedAt: recall('dismissedAt'),
            seenAt,
            seenForMs: seenAt ? Date.now() - Number(recall('seenMs') || 0) : 0,
        });

        let html = '';
        if (shown.outcome) html += outcomeHtml(shown.outcome);
        if (shown.primary) html += primaryHtml(shown.primary);

        // Remember when good news was first shown, so it goes away after a few seconds even across reloads
        if (shown.outcome && shown.outcome.kind === 'ok' && seenAt !== shown.outcome.outcome.at) {
            remember('seenAt', shown.outcome.outcome.at);
            remember('seenMs', String(Date.now()));
        }

        // Only touch the DOM when something changed, so buttons keep focus between polls
        if (host.dataset.html !== html) {
            host.innerHTML = html;
            host.dataset.html = html;
        }
        host.classList.toggle('hidden', html === '');
    }

    // ---- countdown ----------------------------------------------------------------------

    function tick() {
        const el = document.getElementById('config-countdown');
        if (!el || deadlineMs === null) return;
        const left = Math.max(0, Math.ceil((deadlineMs - Date.now()) / 1000));
        el.textContent = String(left);
    }

    // ---- polling ------------------------------------------------------------------------

    async function refresh() {
        const res = await apiFetch('/api/v1/config/status');
        if (!res || !res.ok) {
            lost = true;
        } else {
            lost = false;
            status = await res.json();
            deadlineMs = status.state === 'awaiting_confirm' && status.confirm
                ? Date.now() + status.confirm.seconds_left * 1000
                : null;
            if (status.state === 'idle') forget('pendingUrl');
        }
        render();
        schedule();
    }

    function schedule() {
        clearTimeout(pollTimer);
        let wait = POLL_IDLE_MS;
        if (lost) wait = POLL_LOST_MS;
        else if (status && (status.state === 'applying' || status.state === 'awaiting_confirm')) wait = POLL_BUSY_MS;
        pollTimer = setTimeout(refresh, wait);
    }

    // ---- actions ------------------------------------------------------------------------

    async function post(path) {
        const res = await apiFetch(path, { method: 'POST' });
        if (!res) return { ok: false, error: 'Could not reach the router.' };
        let body = null;
        try { body = await res.json(); } catch (e) { /* no body */ }
        return { ok: res.ok, status: res.status, body };
    }

    async function discard() {
        const n = status ? status.change_count : 0;
        if (!window.confirm(`Discard ${plural(n, 'uncommitted change', 'uncommitted changes')}? This cannot be undone.`)) return;
        await post('/api/v1/config/discard');
        refresh();
    }

    async function confirmChange() {
        const r = await post('/api/v1/config/confirm');
        if (!r.ok) window.alert((r.body && r.body.error) || r.error || 'Could not confirm the change.');
        refresh();
    }

    async function revertChange() {
        if (!window.confirm('Revert this change now?')) return;
        const r = await post('/api/v1/config/revert');
        if (!r.ok) window.alert((r.body && r.body.error) || r.error || 'Could not revert the change.');
        refresh();
    }

    function dismissOutcome(at) {
        remember('dismissedAt', at);
        render();
    }

    // ---- review dialog ------------------------------------------------------------------

    function reviewHtml(rev) {
        const parts = [];
        parts.push('<h2 id="config-review-title">Review changes</h2>');

        if (!rev.changes || rev.changes.length === 0) {
            parts.push('<p class="config-review-empty">There is nothing to commit.</p>');
            return parts.join('');
        }

        parts.push('<h3>What will change</h3><table class="config-review-table"><tbody>' +
            rev.changes.map((c) => `<tr><th scope="row">${esc(c.label)}</th>
                <td><span class="config-old">${esc(c.old)}</span> <span class="config-arrow" aria-hidden="true">&rarr;</span>
                <span class="config-new">${esc(c.new)}</span></td></tr>`).join('') + '</tbody></table>');

        if (rev.errors && rev.errors.length) {
            parts.push('<div class="config-review-errors" role="alert"><strong>These settings are not valid, so they cannot be committed:</strong><ul>' +
                rev.errors.map((e) => `<li>${esc(e.message)}</li>`).join('') + '</ul></div>');
        } else if (rev.problem) {
            parts.push(`<div class="config-review-errors" role="alert"><strong>This cannot be committed:</strong> ${esc(rev.problem)}</div>`);
        }

        if (rev.plan) {
            if (rev.plan.files && rev.plan.files.length) {
                parts.push('<h3>Files that will be written</h3><ul class="config-review-list">' +
                    rev.plan.files.map((f) => `<li>${esc(FILE_KIND_TEXT[f.kind] || f.kind)} <code>${esc(f.path)}</code></li>`).join('') + '</ul>');
            }
            if (rev.plan.actions && rev.plan.actions.length) {
                parts.push('<h3>Services affected</h3><ul class="config-review-list">' +
                    rev.plan.actions.map((a) => `<li>${esc(SERVICE_TEXT[a.service] || (a.verb + ' ' + a.service))}</li>`).join('') + '</ul>');
            }
            if (!(rev.plan.files && rev.plan.files.length) && !(rev.plan.actions && rev.plan.actions.length)) {
                parts.push('<p class="config-review-empty">The system already matches this configuration, so nothing needs restarting.</p>');
            }
        }
        (rev.notes || []).forEach((n) => parts.push(`<p class="config-review-note">${esc(n)}</p>`));

        if (rev.valid && rev.needs_confirm) {
            const ip = rev.changes.find((c) => c.field === 'lan_ip');
            const reconnect = ip
                ? ` Because the router's LAN address changes, you will need to open <strong>https://${esc(ip.new)}</strong> and log in again to confirm.`
                : '';
            const portsChanged = rev.changes.some((c) => c.field === 'lan_interfaces' || c.field === 'wan_interface');
            const replug = portsChanged
                ? ' Because ports change roles, the cable you are connected through may stop working: plug into a LAN port to confirm.'
                : '';
            parts.push(`<p class="config-review-warning" role="note"><strong>You will have ${esc(rev.confirm_seconds)} seconds to confirm.</strong>
                This change affects the network or firewall, so if you do not confirm it is reverted automatically.${reconnect}${replug}</p>`);
        }

        parts.push('<p class="config-review-result hidden" id="config-review-result" role="alert"></p>');
        return parts.join('');
    }

    async function openReview() {
        const dialog = document.getElementById('config-review');
        const body = document.getElementById('config-review-body');
        const commit = document.getElementById('config-review-commit');
        if (!dialog || !body || !commit) return;

        body.innerHTML = '<p>Loading&hellip;</p>';
        commit.disabled = true;
        if (!dialog.open) dialog.showModal();

        const res = await apiFetch('/api/v1/config/staged');
        if (!res || !res.ok) {
            body.innerHTML = '<p class="config-review-errors">Could not load the changes.</p>';
            return;
        }
        reviewing = await res.json();
        body.innerHTML = reviewHtml(reviewing);
        commit.disabled = !(reviewing.valid && reviewing.changes && reviewing.changes.length);
        commit.textContent = reviewing.needs_confirm ? `Commit (confirm within ${reviewing.confirm_seconds}s)` : 'Commit';
    }

    function showReviewResult(message) {
        const el = document.getElementById('config-review-result');
        if (!el) return;
        el.textContent = message;
        el.classList.remove('hidden');
    }

    async function commit() {
        const dialog = document.getElementById('config-review');
        const button = document.getElementById('config-review-commit');
        if (!button) return;

        button.disabled = true;
        const r = await post('/api/v1/config/commit');
        if (!r.ok) {
            const msg = (r.body && r.body.error) || r.error || 'The commit failed.';
            showReviewResult(msg);
            button.disabled = false;
            return;
        }

        // Remember where to reconnect in case the LAN address is about to change under us
        const ip = reviewing && reviewing.changes ? reviewing.changes.find((c) => c.field === 'lan_ip') : null;
        if (ip) remember('pendingUrl', 'https://' + ip.new); else forget('pendingUrl');

        if (dialog && dialog.open) dialog.close();
        refresh();
    }

    // ---- wiring -------------------------------------------------------------------------

    function init() {
        const host = document.getElementById('config-banner');
        if (!host) return;

        host.addEventListener('click', (e) => {
            const btn = e.target.closest('[data-config-action]');
            if (!btn) return;
            switch (btn.dataset.configAction) {
                case 'review': openReview(); break;
                case 'discard': discard(); break;
                case 'confirm': confirmChange(); break;
                case 'revert': revertChange(); break;
                case 'dismiss': dismissOutcome(btn.dataset.at); break;
            }
        });

        const dialog = document.getElementById('config-review');
        if (dialog) {
            document.getElementById('config-review-commit').addEventListener('click', commit);
            document.getElementById('config-review-cancel').addEventListener('click', () => dialog.close());
        }

        setInterval(tick, 250);
        refresh();

        // right after first-run setup the settings are a draft: show what will change straight away
        let fresh = false;
        try { fresh = sessionStorage.getItem('pr_open_review') === '1'; sessionStorage.removeItem('pr_open_review'); } catch (e) { fresh = false; }
        if (fresh) openReview();
    }

    document.addEventListener('DOMContentLoaded', init);

    // describe and render are exposed so the banner can be checked from the browser console
    return { describe, render: (st) => { status = st; render(); }, refresh };
})();
