import { useRef, useState } from "react";

import { cn } from "../../lib/cn";

export type AvatarSize = "sm" | "md" | "lg";

export interface AvatarProps {
  /** Full name or email used to derive initials and a stable colour. */
  name: string;
  /** Optional image URL; falls back to initials if it fails to load. */
  src?: string;
  size?: AvatarSize;
  className?: string;
}

/** Derive up to two uppercase initials from a name or email. */
export function initialsFromName(name: string): string {
  const trimmed = name.trim();
  if (!trimmed) return "?";
  // For email-like inputs, use the local part before "@".
  const base = trimmed.includes("@") ? trimmed.split("@")[0] : trimmed;
  const parts = base.split(/[\s._-]+/).filter(Boolean);
  if (parts.length === 0) return base.slice(0, 2).toUpperCase();
  if (parts.length === 1) return parts[0].slice(0, 2).toUpperCase();
  return (parts[0][0] + parts[parts.length - 1][0]).toUpperCase();
}

/** Deterministic hue from a string so a given user is always the
 *  same colour across sessions. */
function hueFromString(value: string): number {
  let hash = 0;
  for (let i = 0; i < value.length; i++) {
    hash = (hash << 5) - hash + value.charCodeAt(i);
    hash |= 0;
  }
  return Math.abs(hash) % 360;
}

const sizeClass: Record<AvatarSize, string> = {
  sm: "size-7 text-[0.7rem]",
  md: "size-9 text-sm",
  lg: "size-12 text-[1.1rem]",
};

/** Avatar — a circular user marker showing an image or initials. */
export function Avatar({
  name,
  src,
  size = "md",
  className,
}: AvatarProps): JSX.Element {
  const [imgFailed, setImgFailed] = useState(false);
  // Reset the failure flag when `src` changes so a new (possibly valid)
  // image gets a chance to load instead of being stuck on initials
  // after the first broken URL. This is the React-recommended
  // adjust-state-during-render pattern (no effect needed).
  const prevSrc = useRef(src);
  if (prevSrc.current !== src) {
    prevSrc.current = src;
    if (imgFailed) setImgFailed(false);
  }
  const showImage = src && !imgFailed;
  const hue = hueFromString(name);

  return (
    <span
      className={cn(
        "relative inline-flex shrink-0 select-none items-center justify-center overflow-hidden rounded-full font-semibold leading-none",
        sizeClass[size],
        className,
      )}
      style={
        showImage
          ? undefined
          : {
              backgroundColor: `hsl(${hue} 65% 45%)`,
              color: "#fff",
            }
      }
      role="img"
      aria-label={name}
      title={name}
    >
      {showImage ? (
        <img
          className="size-full object-cover"
          src={src}
          alt=""
          onError={() => setImgFailed(true)}
        />
      ) : (
        <span aria-hidden="true">{initialsFromName(name)}</span>
      )}
    </span>
  );
}
