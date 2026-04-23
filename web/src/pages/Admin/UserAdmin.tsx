/**
 * UserAdmin is the user management console.
 *
 * In Phase 3 it will drive user provisioning (create, suspend,
 * rotate, delete), alias management, and shared-inbox membership
 * changes via the Tenant Service. A membership change on a
 * shared inbox triggers an MLS epoch change on the backing KChat
 * group (docs/SCHEMA.md §5.6). In Phase 1 it is a placeholder.
 */
export default function UserAdmin() {
  return (
    <section>
      <h2>User admin</h2>
      <p>Not yet implemented.</p>
    </section>
  );
}
