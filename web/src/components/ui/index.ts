/**
 * Barrel export for the KMail UI component library.
 *
 * Import shared primitives from a single path:
 *   `import { Button, Modal, useToast } from "../components/ui";`
 */
export { Button } from "./Button";
export type { ButtonProps, ButtonSize, ButtonVariant } from "./Button";

export { Input } from "./Input";
export type { InputProps } from "./Input";

export { Select } from "./Select";
export type { SelectOption, SelectProps } from "./Select";

export { Modal } from "./Modal";
export type { ModalProps, ModalSize } from "./Modal";

export { Badge } from "./Badge";
export type { BadgeProps, BadgeVariant } from "./Badge";

export { Avatar, initialsFromName } from "./Avatar";
export type { AvatarProps, AvatarSize } from "./Avatar";

export { Card } from "./Card";
export type { CardProps } from "./Card";

export { Table } from "./Table";
export type { TableColumn, TableProps } from "./Table";

export { Tabs } from "./Tabs";
export type { TabItem, TabsProps } from "./Tabs";

export { Dropdown } from "./Dropdown";
export type { DropdownItem, DropdownProps } from "./Dropdown";

export { Tooltip } from "./Tooltip";
export type { TooltipPlacement, TooltipProps } from "./Tooltip";

export { Skeleton } from "./Skeleton";
export type { SkeletonProps } from "./Skeleton";

export { EmptyState } from "./EmptyState";
export type { EmptyStateProps } from "./EmptyState";

export { Toast } from "./Toast";
export type { ToastData, ToastProps, ToastVariant } from "./Toast";
