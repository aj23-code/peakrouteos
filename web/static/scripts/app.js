// web/static/scripts/app.js

/**
 * Universal API Fetch Wrapper
 * Intercepts 401 Unauthorized responses to bounce expired sessions directly to /login.
 */
async function apiFetch(url, options = {}) {
    try {
        // State-changing requests must carry this session's CSRF token (see the page's <meta>)
        const method = (options.method || 'GET').toUpperCase();
        if (!['GET', 'HEAD', 'OPTIONS'].includes(method)) {
            const token = document.querySelector('meta[name="csrf-token"]');
            const headers = new Headers(options.headers || {});
            if (token) headers.set('X-CSRF-Token', token.content);
            options = { ...options, headers };
        }

        const res = await fetch(url, options);

        if (res.status === 401) {
            window.location.href = '/login';
            return null;
        }

        if (res.status === 403) {
            console.warn(`[API] ${method} ${url} was rejected (403): missing or invalid CSRF token. Reload the page.`);
        }

        return res;
    } catch (err) {
        console.error(`[API Error] Request failed for ${url}:`, err);
        return null;
    }
}

/**
 * Formatting Utilities
 */
function formatBytes(bytes) {
    if (!bytes || bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(2)) + ' ' + sizes[i];
}

/**
 * Latency, percentage and bit-rate formatters shared by the Overview and Internet pages.
 */
function formatMs(v) {
    if (v === null || v === undefined) return '\u2013';
    return (v >= 100 ? v.toFixed(0) : v.toFixed(1)) + ' ms';
}

function formatPercent(v) {
    if (v === null || v === undefined) return '\u2013';
    if (v === 100) return '100%';
    // 99.996 must not read as 100 while minutes were lost
    const shown = v > 99.99 ? 99.99 : v;
    return shown.toFixed(shown >= 99 ? 2 : 1) + '%';
}

// Bytes per second shown as the bits per second people expect from a network
function formatRate(bytesPerSec) {
    const bps = (bytesPerSec || 0) * 8;
    if (bps < 1000) return bps.toFixed(0) + ' bps';
    if (bps < 1e6) return (bps / 1e3).toFixed(1) + ' Kbps';
    if (bps < 1e9) return (bps / 1e6).toFixed(1) + ' Mbps';
    return (bps / 1e9).toFixed(2) + ' Gbps';
}

/**
 * Escape a value for safe use in HTML text or a quoted attribute value.
 * Use for anything that ends up in innerHTML and did not come from this codebase
 * (DHCP hostnames in particular are chosen by whatever device is on the LAN).
 */
function escapeHtml(value) {
    return String(value ?? '')
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;')
        .replace(/"/g, '&quot;')
        .replace(/'/g, '&#039;');
}

function formatUptime(seconds) {
    if (!seconds) return '0s';
    const days = Math.floor(seconds / (3600 * 24));
    const hrs = Math.floor((seconds % (3600 * 24)) / 3600);
    const mins = Math.floor((seconds % 3600) / 60);
    const secs = seconds % 60;

    let parts = [];
    if (days > 0) parts.push(`${days}d`);
    if (hrs > 0) parts.push(`${hrs}h`);
    if (mins > 0) parts.push(`${mins}m`);
    parts.push(`${secs}s`);
    return parts.join(' ');
}

/**
 * Subpage Router & View Lifecycle Manager
 */
let activeViewCleanup = null;

function loadView(viewName) {
    // Clean up timers/listeners from the previous view
    if (typeof activeViewCleanup === 'function') {
        activeViewCleanup();
        activeViewCleanup = null;
    }

    // Toggle active link highlight
    document.querySelectorAll('.nav-item').forEach(item => {
        item.classList.remove('active');
    });

    const activeNav = document.querySelector(`.nav-item[data-view="${viewName}"]`);
    if (activeNav) {
        activeNav.classList.add('active');

        // Open the group the page belongs to; close the other groups, except while a search is
        // showing several matching groups at once
        const group = activeNav.closest('details');
        const searchBox = document.getElementById('nav-search');
        if (group && !(searchBox && searchBox.value.trim())) {
            document.querySelectorAll('.nav-banner-group[open]').forEach(g => { if (g !== group) g.open = false; });
        }
        let ancestor = group;
        while (ancestor) {
            ancestor.open = true;
            ancestor = ancestor.parentElement ? ancestor.parentElement.closest('details') : null;
        }
    }

    // Toggle main view section visibility
    document.querySelectorAll('.view-section').forEach(section => {
        section.classList.add('hidden');
    });
    const targetSection = document.getElementById(`view-${viewName}`);
    if (targetSection) {
        targetSection.classList.remove('hidden');

        // On narrow screens the top bar names the page you are on
        const title = targetSection.querySelector('h1');
        const appbarTitle = document.getElementById('appbar-title');
        if (title && appbarTitle) appbarTitle.textContent = title.textContent;
    }

    // Trigger view initialization module
    switch (viewName) {
        case 'overview':
            if (window.TelemetryModule) {
                activeViewCleanup = window.TelemetryModule.init();
            }
            break;
        case 'traffic':
            if (window.TrafficModule) {
                activeViewCleanup = window.TrafficModule.init();
            }
            break;
        case 'logs':
            if (window.SysLogsModule) {
                activeViewCleanup = window.SysLogsModule.init();
            }
            break;
        case 'dhcp':
            if (window.DHCPModule) {
                activeViewCleanup = window.DHCPModule.init();
            }
            break;
        case 'internet':
            if (window.InternetModule) {
                activeViewCleanup = window.InternetModule.init();
            }
            break;
        case 'network-physical':
            if (window.PortsModule) {
                activeViewCleanup = window.PortsModule.init();
            }
            break;
        case 'wireless':
            if (window.WirelessModule) {
                activeViewCleanup = window.WirelessModule.init();
            }
            break;
        case 'routes':
            if (window.RoutesModule) {
                activeViewCleanup = window.RoutesModule.init();
            }
            break;
        case 'firewall-rules':
            if (window.FirewallRulesModule) {
                activeViewCleanup = window.FirewallRulesModule.init();
            }
            break;
        case 'firewall-forwards':
            if (window.FirewallForwardsModule) {
                activeViewCleanup = window.FirewallForwardsModule.init();
            }
            break;
        case 'firewall-connections':
            if (window.FirewallConnectionsModule) {
                activeViewCleanup = window.FirewallConnectionsModule.init();
            }
            break;
        case 'firewall-log':
            if (window.FirewallLogModule) {
                activeViewCleanup = window.FirewallLogModule.init();
            }
            break;
        case 'dns':
            if (window.DNSModule) {
                activeViewCleanup = window.DNSModule.init();
            }
            break;
        case 'diagnostics':
            if (window.DiagnosticsModule) {
                activeViewCleanup = window.DiagnosticsModule.init();
            }
            break;
        case 'settings':
            if (window.SettingsModule) {
                activeViewCleanup = window.SettingsModule.init();
            }
            break;
        case 'networks':
            if (window.NetworksModule) {
                activeViewCleanup = window.NetworksModule.init();
            }
            break;
        default:
            console.warn(`No view module configured for: ${viewName}`);
    }
}

/**
 * The slide-out menu used on narrow screens (the stylesheet decides when it is a drawer; on a wide
 * screen these controls are hidden and the sidebar is always visible).
 */
const NARROW_SCREEN = window.matchMedia('(max-width: 900px)');

function setMobileNav(open) {
    const toggle = document.getElementById('nav-toggle');
    const sidebar = document.getElementById('sidebar');
    if (!toggle || !sidebar) return;

    const wasOpen = document.body.classList.contains('nav-open');
    document.body.classList.toggle('nav-open', open);
    toggle.setAttribute('aria-expanded', String(open));
    toggle.setAttribute('aria-label', open ? 'Close menu' : 'Open menu');

    // Keyboard users land in the menu when it opens and back on the button when it closes
    if (open && !wasOpen) sidebar.focus();
    if (!open && wasOpen) toggle.focus();
}

function closeMobileNav() {
    if (NARROW_SCREEN.matches) setMobileNav(false);
}

function initMobileNav() {
    const toggle = document.getElementById('nav-toggle');
    if (!toggle) return;

    toggle.addEventListener('click', () => setMobileNav(!document.body.classList.contains('nav-open')));
    document.getElementById('nav-close').addEventListener('click', () => setMobileNav(false));
    document.getElementById('nav-backdrop').addEventListener('click', () => setMobileNav(false));
    document.addEventListener('keydown', (e) => {
        if (e.key === 'Escape' && document.body.classList.contains('nav-open')) setMobileNav(false);
    });

    // Growing the window past the breakpoint shows the normal sidebar: drop the open state
    NARROW_SCREEN.addEventListener('change', (e) => {
        if (!e.matches) {
            document.body.classList.remove('nav-open');
            toggle.setAttribute('aria-expanded', 'false');
        }
    });
}

/**
 * Quick search above the menu: filters the entries (a group's own name matches all of its pages),
 * opens the groups that have matches, and puts the menu back the way it was when the box is cleared.
 */
function initNavSearch(nav) {
    const input = document.getElementById('nav-search');
    if (!input) return;

    let openBefore = null; // which groups were open before the search started

    function apply() {
        const term = input.value.trim().toLowerCase();
        const groups = [...nav.querySelectorAll('.nav-banner-group')];

        if (term && openBefore === null) openBefore = groups.map(g => g.open);

        nav.querySelectorAll(':scope > .nav-item').forEach(item => {
            item.classList.toggle('hidden', !!term && !item.textContent.toLowerCase().includes(term));
        });

        groups.forEach(group => {
            const titleHit = group.querySelector('.banner-title').textContent.toLowerCase().includes(term);
            let anyVisible = false;
            group.querySelectorAll('.nav-item').forEach(item => {
                const hit = !term || titleHit || item.textContent.toLowerCase().includes(term);
                item.classList.toggle('hidden', !hit);
                anyVisible = anyVisible || hit;
            });
            group.classList.toggle('hidden', !!term && !anyVisible);
            if (term && anyVisible) group.open = true;
        });

        // Nothing matched: say so instead of leaving an empty menu
        const empty = document.getElementById('nav-empty');
        if (empty) {
            const anyShown = nav.querySelector(':scope > .nav-item:not(.hidden)') || nav.querySelector('.nav-banner-group:not(.hidden)');
            empty.classList.toggle('hidden', !term || !!anyShown);
        }

        // Search cleared: show the group of the page you are on (you may have clicked a result),
        // or, if you are on a top-level page, the groups that were open before the search
        if (!term && openBefore !== null) {
            const activeGroup = nav.querySelector('.nav-item.active')?.closest('.nav-banner-group');
            groups.forEach((group, i) => { group.open = activeGroup ? group === activeGroup : openBefore[i]; });
            openBefore = null;
        }
    }

    input.addEventListener('input', apply);
    input.addEventListener('keydown', (e) => {
        if (e.key === 'Escape') {
            input.value = '';
            apply();
        }
    });
}

// Links inside pages that jump to another page: <a href="#" data-goto="network-physical">
document.addEventListener('click', (e) => {
    const link = e.target.closest('[data-goto]');
    if (!link) return;
    e.preventDefault();
    loadView(link.dataset.goto);
});

// App Shell Initialization
document.addEventListener('DOMContentLoaded', () => {
    const sidebarNav = document.getElementById('sidebar-nav');

    if (sidebarNav) {
        // Event delegation for sidebar link navigation
        sidebarNav.addEventListener('click', (e) => {
            const navLink = e.target.closest('.nav-item[data-view]');
            if (!navLink) return;

            e.preventDefault();
            const view = navLink.getAttribute('data-view');
            loadView(view);
            closeMobileNav();
        });

        // Accordion behavior: when a person opens a group, close the others. Only clicks on a group
        // header count: groups the page opens by itself (the active view, a search) leave the rest alone.
        let userToggled = false;
        sidebarNav.addEventListener('click', (e) => {
            if (e.target.closest('summary')) userToggled = true;
        }, true);
        sidebarNav.addEventListener('toggle', (e) => {
            if (!userToggled) return;
            userToggled = false;
            if (!e.target.classList.contains('nav-banner-group') || !e.target.open) return;

            sidebarNav.querySelectorAll('.nav-banner-group[open]').forEach(banner => {
                if (banner !== e.target) {
                    banner.open = false;
                }
            });
        }, true);

        initNavSearch(sidebarNav);
    }

    initMobileNav();

    // Load default initial view
    loadView('overview');
});
