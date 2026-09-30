// web/static/scripts/app.js

/**
 * Universal API Fetch Wrapper
 * Intercepts 401 Unauthorized responses to bounce expired sessions directly to /login.
 */
async function apiFetch(url, options = {}) {
    try {
        const res = await fetch(url, options);

        if (res.status === 401) {
            window.location.href = '/login';
            return null;
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

        // Automatically expand parent banners & sub-folders when navigated to programmatically
        let ancestor = activeNav.closest('details');
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
        default:
            console.warn(`No view module configured for: ${viewName}`);
    }
}

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
        });

        // Accordion behavior: auto-close other top-level banners when a new banner opens
        sidebarNav.addEventListener('toggle', (e) => {
            if (!e.target.classList.contains('nav-banner-group') || !e.target.open) return;

            sidebarNav.querySelectorAll('.nav-banner-group[open]').forEach(banner => {
                if (banner !== e.target) {
                    banner.open = false;
                }
            });
        }, true);
    }

    // Load default initial view
    loadView('overview');
});
