import type { HTMLAttributes, ReactNode } from "react";

import styles from "./Card.module.css";

export interface CardProps
  extends Omit<HTMLAttributes<HTMLDivElement>, "title"> {
  /** Optional header content rendered above a divider. */
  title?: ReactNode;
  /** Optional actions shown on the right of the header. */
  actions?: ReactNode;
  /** Remove body padding (for tables / media that bleed to the edge). */
  flush?: boolean;
  children: ReactNode;
}

function cx(...classes: Array<string | false | undefined>): string {
  return classes.filter(Boolean).join(" ");
}

/** Card — a surface container with an optional titled header. */
export function Card({
  title,
  actions,
  flush = false,
  className,
  children,
  ...rest
}: CardProps): JSX.Element {
  return (
    <div className={cx(styles.card, className)} {...rest}>
      {(title || actions) && (
        <div className={styles.header}>
          {title && <h3 className={styles.title}>{title}</h3>}
          {actions && <div className={styles.actions}>{actions}</div>}
        </div>
      )}
      <div className={cx(styles.body, flush && styles.flush)}>{children}</div>
    </div>
  );
}
