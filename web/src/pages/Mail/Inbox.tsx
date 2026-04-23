/**
 * Inbox is the primary Mail list view.
 *
 * In Phase 2 this page issues JMAP `Email/query` + `Email/get`
 * through the Go BFF (docs/JMAP-CONTRACT.md §6) and subscribes to
 * mailbox push state changes over WebSocket (§5). In Phase 1 it is
 * a placeholder so the router is well-formed.
 */
export default function Inbox() {
  return (
    <section>
      <h2>Inbox</h2>
      <p>Not yet implemented.</p>
    </section>
  );
}
