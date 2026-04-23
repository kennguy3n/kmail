/**
 * MessageView is the single-message reading pane.
 *
 * In Phase 2 it will render one `Email` (`Email/get` with a full
 * property set) plus inline attachments. For Vault mailboxes it
 * will decrypt the StrictZK blob client-side using MLS-derived
 * folder keys (docs/JMAP-CONTRACT.md §2.4). In Phase 1 it is a
 * placeholder.
 */
export default function MessageView() {
  return (
    <section>
      <h2>Message</h2>
      <p>Not yet implemented.</p>
    </section>
  );
}
