// Inline stroke icons (24px grid, drawn at 16px) so the UI needs no icon
// font or library. Decorative: every use sits next to a text label or an
// aria-label on its control.
const paths: Record<string, React.ReactNode> = {
  workflows: (
    <>
      <rect x="3" y="3" width="6" height="6" rx="1.5" />
      <rect x="15" y="15" width="6" height="6" rx="1.5" />
      <rect x="15" y="3" width="6" height="6" rx="1.5" />
      <path d="M9 6h6M18 9v6M6 9v5a4 4 0 0 0 4 4h5" />
    </>
  ),
  runs: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="m10 8.5 5 3.5-5 3.5z" />
    </>
  ),
  deliveries: (
    <>
      <path d="M22 12h-6l-2 3h-4l-2-3H2" />
      <path d="M5.5 5.1 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.5-6.9A2 2 0 0 0 16.7 4H7.3a2 2 0 0 0-1.8 1.1z" />
    </>
  ),
  integrations: (
    <>
      <path d="M12 22v-5M9 8V2M15 8V2" />
      <path d="M18 8v3a6 6 0 0 1-12 0V8z" />
    </>
  ),
  usage: <path d="M3 3v18h18M8 17v-5M13 17V8M18 17v-8" />,
  hosts: (
    <>
      <rect x="3" y="4" width="18" height="7" rx="1.5" />
      <rect x="3" y="13" width="18" height="7" rx="1.5" />
      <path d="M7 7.5h.01M7 16.5h.01" />
    </>
  ),
  external: <path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5" />,
  plus: <path d="M12 5v14M5 12h14" />,
  play: <path d="M7 4.5v15l12-7.5z" />,
  refresh: (
    <>
      <path d="M21 12a9 9 0 0 1-15.5 6.2L3 16M3 12a9 9 0 0 1 15.5-6.2L21 8" />
      <path d="M21 3v5h-5M3 21v-5h5" />
    </>
  ),
  logout: <path d="M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9" />,
  stop: <rect x="6" y="6" width="12" height="12" rx="2" />,
  rerun: (
    <>
      <path d="M3 12a9 9 0 1 0 3-6.7L3 8" />
      <path d="M3 3v5h5" />
    </>
  ),
  check: <path d="M20 6 9 17l-5-5" />,
  commit: (
    <>
      <circle cx="12" cy="12" r="3.5" />
      <path d="M3 12h5.5M15.5 12H21" />
    </>
  ),
};

export type IconName = keyof typeof paths;

export function Icon({ name }: { name: IconName }) {
  return (
    <svg className="icon" viewBox="0 0 24 24" aria-hidden="true" focusable="false">
      {paths[name]}
    </svg>
  );
}

// The shared Icarus mark is generated alongside the native application icons.
export function Logo() {
  return (
    <img className="logo" src={`${import.meta.env.BASE_URL}icaro.svg`} alt="" aria-hidden="true" />
  );
}
