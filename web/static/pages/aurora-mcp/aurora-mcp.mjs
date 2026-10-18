import { html, css } from 'https://cdn.jsdelivr.net/gh/lit/dist@3/all/lit-all.min.js';
import { StyledLitElement } from '/ux/StyledLitElement.mjs';
import RPCCall from '/lib/jsonrpc.mjs';

const PAGE = 100;

// Read-only diagnostics MCP (aurora/mcp): configured agents and the tool-call
// log. Approvals for side-effecting tools will live here later.
class AuroraMCP extends StyledLitElement {
    static properties = {
        status: { type: Object },
        calls: { type: Array },
        agent: { type: String },
        tool: { type: String },
        expanded: { type: Object },
        error: { type: String },
        more: { type: Boolean },
    };

    static styles = css`
        :host { display: block; }
        .filters { display: flex; gap: 12px; align-items: center; margin: 12px 0; }
        .mono { font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace; font-size: 12px; }
        pre { white-space: pre-wrap; word-break: break-all; margin: 0; }
        tr.call { cursor: pointer; }
        .err { color: #f77; }
        .muted { color: #999; }
        .badge-off { color: #f77; }
        .badge-on { color: #7f7; }
    `;

    constructor() {
        super();
        this.calls = [];
        this.agent = '';
        this.tool = '';
        this.expanded = {};
        this.more = false;
    }

    connectedCallback() {
        super.connectedCallback();
        this.reload();
        this._timer = setInterval(() => this.refreshNewest(), 10000);
    }

    disconnectedCallback() {
        super.disconnectedCallback();
        clearInterval(this._timer);
    }

    async reload() {
        try {
            this.error = '';
            this.status = await RPCCall('AuroraMCPStatus', []);
            const calls = await RPCCall('AuroraMCPCalls', [PAGE, this.agent, this.tool, 0]) || [];
            this.calls = calls;
            this.more = calls.length === PAGE;
        } catch (e) {
            this.error = String(e);
        }
    }

    async refreshNewest() {
        try {
            const calls = await RPCCall('AuroraMCPCalls', [PAGE, this.agent, this.tool, 0]) || [];
            const known = new Set(this.calls.map(c => c.ID));
            const fresh = calls.filter(c => !known.has(c.ID));
            if (fresh.length) this.calls = [...fresh, ...this.calls];
        } catch (e) {
            // keep the current view; the next tick retries
        }
    }

    async loadOlder() {
        const last = this.calls[this.calls.length - 1];
        if (!last) return;
        const calls = await RPCCall('AuroraMCPCalls', [PAGE, this.agent, this.tool, last.ID]) || [];
        this.calls = [...this.calls, ...calls];
        this.more = calls.length === PAGE;
    }

    toggle(id) {
        this.expanded = { ...this.expanded, [id]: !this.expanded[id] };
    }

    fmtArgs(a) {
        try { return JSON.stringify(JSON.parse(a), null, 2); } catch (e) { return a; }
    }

    renderStatus() {
        const st = this.status;
        if (!st) return html``;
        return html`
            <p>MCP endpoint on this node:
                ${st.Enabled
                    ? html`<span class="badge-on">enabled</span> at <span class="mono">${location.origin}/mcp</span>`
                    : html`<span class="badge-off">disabled</span> (set Aurora.MCP.Enable)`}
            </p>
            <table class="table table-dark table-sm">
                <thead><tr><th>Agent</th><th>Token</th><th>Last call</th><th>Calls (24h)</th></tr></thead>
                <tbody>
                ${(st.Agents || []).length === 0 ? html`<tr><td colspan="4" class="muted">No agent tokens configured (Aurora.MCP.Tokens)</td></tr>` : ''}
                ${(st.Agents || []).map(a => html`
                    <tr>
                        <td>${a.Name || html`<span class="muted">(unnamed)</span>`}</td>
                        <td>${!a.Valid ? html`<span class="err">invalid entry</span>` : a.Disabled ? html`<span class="badge-off">disabled</span>` : html`<span class="badge-on">active</span>`}</td>
                        <td>${a.LastCall ? new Date(a.LastCall).toLocaleString() : html`<span class="muted">never</span>`}</td>
                        <td>${a.Calls24h}</td>
                    </tr>`)}
                </tbody>
            </table>`;
    }

    render() {
        const agents = (this.status?.Agents || []).map(a => a.Name);
        return html`
            <h2>Diagnostics MCP</h2>
            ${this.error ? html`<p class="err">${this.error}</p>` : ''}
            ${this.renderStatus()}

            <h3>Tool calls</h3>
            <div class="filters">
                <label>Agent
                    <select @change=${e => { this.agent = e.target.value; this.reload(); }}>
                        <option value="">all</option>
                        ${agents.map(a => html`<option value=${a} ?selected=${a === this.agent}>${a}</option>`)}
                    </select>
                </label>
                <label>Tool
                    <input class="mono" placeholder="e.g. log_grep" .value=${this.tool}
                           @change=${e => { this.tool = e.target.value.trim(); this.reload(); }}>
                </label>
                <span class="muted">Click a row for arguments and errors. All tools are read-only.</span>
            </div>
            <table class="table table-dark table-sm">
                <thead><tr><th>Time</th><th>Agent</th><th>Tool</th><th>Duration</th><th>Result</th><th>Status</th></tr></thead>
                <tbody>
                ${this.calls.map(c => html`
                    <tr class="call" @click=${() => this.toggle(c.ID)}>
                        <td>${new Date(c.TS).toLocaleString()}</td>
                        <td>${c.Agent}</td>
                        <td class="mono">${c.Tool}</td>
                        <td>${c.DurationMS} ms</td>
                        <td>${(c.ResultBytes / 1024).toFixed(1)} KiB${c.Truncated ? html` <span class="err">(truncated)</span>` : ''}</td>
                        <td>${c.Error ? html`<span class="err">error</span>` : 'ok'}</td>
                    </tr>
                    ${this.expanded[c.ID] ? html`
                        <tr><td colspan="6" class="mono">
                            <pre>${this.fmtArgs(c.Args)}</pre>
                            ${c.Error ? html`<pre class="err">${c.Error}</pre>` : ''}
                        </td></tr>` : ''}
                `)}
                </tbody>
            </table>
            ${this.more ? html`<button class="btn btn-secondary" @click=${() => this.loadOlder()}>Older</button>` : ''}
        `;
    }
}

customElements.define('aurora-mcp', AuroraMCP);
