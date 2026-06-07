import { useState } from "react";
import { Copy, Inbox, Moon, Pencil, Sun, Trash2 } from "lucide-react";

import {
  Avatar,
  Badge,
  Button,
  Card,
  Dropdown,
  EmptyState,
  Input,
  Modal,
  Select,
  Skeleton,
  Table,
  Tabs,
  Tooltip,
} from "./ui";
import { useTheme } from "../hooks/useTheme";
import { useToast } from "./ToastProvider";

interface DemoRow {
  domain: string;
  status: "Verified" | "Pending";
  users: number;
}

const DEMO_ROWS: DemoRow[] = [
  { domain: "acme.example", status: "Verified", users: 128 },
  { domain: "globex.example", status: "Pending", users: 12 },
  { domain: "initech.example", status: "Verified", users: 47 },
];

/**
 * Showcase — a non-production gallery of the KMail UI component
 * library. Reachable at `/showcase` for visual review, design QA,
 * and before/after screenshots. It deliberately exercises every
 * shared primitive in both themes.
 */
export default function Showcase(): JSX.Element {
  const { resolvedTheme, toggleTheme } = useTheme();
  const toast = useToast();
  const [modalOpen, setModalOpen] = useState(false);

  return (
    <div className="flex flex-col gap-6">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-semibold">KMail Design System</h1>
          <p className="mt-1 text-fg-muted">
            Shared component library &amp; design tokens (WS1).
          </p>
        </div>
        <Button
          onClick={toggleTheme}
          variant="secondary"
          iconLeft={resolvedTheme === "dark" ? <Sun /> : <Moon />}
        >
          {resolvedTheme === "dark" ? "Light mode" : "Dark mode"}
        </Button>
      </header>

      <div className="grid grid-cols-[repeat(auto-fill,minmax(20rem,1fr))] items-start gap-4">
        <Card title="Buttons">
          <div className="flex flex-col gap-3">
            <div className="flex flex-wrap items-center gap-3">
              <Button variant="primary">Primary</Button>
              <Button variant="secondary">Secondary</Button>
              <Button variant="ghost">Ghost</Button>
              <Button variant="danger">Danger</Button>
              <Button variant="link">Link</Button>
            </div>
            <div className="flex flex-wrap items-center gap-3">
              <Button size="sm">Small</Button>
              <Button size="md">Medium</Button>
              <Button size="lg">Large</Button>
              <Button loading>Loading</Button>
              <Button disabled>Disabled</Button>
            </div>
          </div>
        </Card>

        <Card title="Badges">
          <div className="flex flex-wrap items-center gap-3">
            <Badge variant="neutral">Neutral</Badge>
            <Badge variant="primary">Primary</Badge>
            <Badge variant="success" dot>
              Active
            </Badge>
            <Badge variant="warning">Warning</Badge>
            <Badge variant="danger">Error</Badge>
            <Badge variant="info">Info</Badge>
          </div>
        </Card>

        <Card title="Form controls">
          <div className="flex flex-col gap-3">
            <Input label="Email" placeholder="you@example.com" />
            <Input
              label="Password"
              type="password"
              error="Must be at least 12 characters"
            />
            <Select
              label="Plan"
              options={[
                { value: "starter", label: "Starter" },
                { value: "business", label: "Business" },
                { value: "privacy", label: "Privacy" },
              ]}
            />
          </div>
        </Card>

        <Card title="Avatars & overlays">
          <div className="flex flex-wrap items-center gap-3">
            <Avatar name="Ada Lovelace" size="sm" />
            <Avatar name="Grace Hopper" size="md" />
            <Avatar name="alan@example.com" size="lg" />
            <Tooltip label="Helpful hint shown on hover/focus">
              <Button variant="secondary">Hover me</Button>
            </Tooltip>
            <Dropdown
              ariaLabel="Demo menu"
              trigger={<Button variant="secondary">Menu ▾</Button>}
              items={[
                { id: "1", label: "Edit", icon: <Pencil /> },
                { id: "2", label: "Duplicate", icon: <Copy /> },
                {
                  id: "3",
                  label: "Delete",
                  icon: <Trash2 />,
                  danger: true,
                  separatorBefore: true,
                },
              ]}
            />
          </div>
        </Card>

        <Card title="Toasts">
          <div className="flex flex-wrap items-center gap-3">
            <Button
              variant="secondary"
              onClick={() => toast.success("Saved successfully")}
            >
              Success
            </Button>
            <Button
              variant="secondary"
              onClick={() =>
                toast.error("Something went wrong", { title: "Error" })
              }
            >
              Error
            </Button>
            <Button
              variant="secondary"
              onClick={() => toast.warning("Quota almost reached")}
            >
              Warning
            </Button>
            <Button variant="secondary" onClick={() => toast.info("Heads up")}>
              Info
            </Button>
          </div>
        </Card>

        <Card title="Modal">
          <Button onClick={() => setModalOpen(true)}>Open modal</Button>
          <Modal
            open={modalOpen}
            onClose={() => setModalOpen(false)}
            title="Confirm action"
            footer={
              <>
                <Button variant="ghost" onClick={() => setModalOpen(false)}>
                  Cancel
                </Button>
                <Button variant="primary" onClick={() => setModalOpen(false)}>
                  Confirm
                </Button>
              </>
            }
          >
            <p>
              This is an accessible modal dialog with a focus trap,
              Escape-to-close, and a restored focus target.
            </p>
          </Modal>
        </Card>

        <Card title="Tabs">
          <Tabs
            ariaLabel="Demo tabs"
            items={[
              { id: "general", label: "General", content: <p>General settings.</p> },
              { id: "security", label: "Security", content: <p>Security settings.</p> },
              { id: "billing", label: "Billing", content: <p>Billing settings.</p> },
            ]}
          />
        </Card>

        <Card title="Table" flush>
          <Table<DemoRow>
            caption="Demo domains"
            rowKey={(r) => r.domain}
            columns={[
              { key: "domain", header: "Domain", render: (r) => r.domain },
              {
                key: "status",
                header: "Status",
                render: (r) => (
                  <Badge variant={r.status === "Verified" ? "success" : "warning"}>
                    {r.status}
                  </Badge>
                ),
              },
              {
                key: "users",
                header: "Users",
                align: "right",
                render: (r) => r.users,
              },
            ]}
            rows={DEMO_ROWS}
          />
        </Card>

        <Card title="Skeleton (loading)">
          <Skeleton label="Loading content" lines={4} />
        </Card>

        <Card title="Empty state">
          <EmptyState
            icon={<Inbox />}
            title="No messages"
            description="Your inbox is empty. New mail will appear here."
            action={<Button variant="primary">Compose</Button>}
          />
        </Card>
      </div>
    </div>
  );
}
