/** @type {import('tailwindcss').Config} */
export default {
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        // Deep petrol/teal — the primary color. Deliberately NOT the
        // generic SaaS blue every db tool reaches for; evokes depth /
        // reservoir without copying PostgreSQL's own elephant-blue brand.
        petrol: {
          50: "#eef5f4",
          100: "#d3e6e3",
          200: "#a7cdc8",
          300: "#7bb3ac",
          400: "#4f9a90",
          500: "#2f7d73",
          600: "#22645c",
          700: "#1a4c46",
          800: "#123430",
          900: "#0b201d",
          950: "#061312",
        },
        // Amber — reserved for "active / in progress" states (the
        // current phase in a timeline). Never used for anything else.
        amber: {
          50: "#fdf6e9",
          100: "#f9e6bf",
          200: "#f2ca7a",
          300: "#e8ab3f",
          400: "#d4901f",
          500: "#b3760f",
          600: "#8c5c0c",
        },
        // Coral/red — reserved STRICTLY for destructive warnings, matching
        // the backend's own Warnings-vs-Notes distinction in
        // internal/preview.Report. Never used decoratively.
        coral: {
          50: "#fdeeec",
          100: "#f9d2cc",
          200: "#f2a99c",
          300: "#e87a63",
          400: "#d8523a",
          500: "#b53c26",
          600: "#8f2e1c",
        },
        ink: {
          50: "#f4f5f5",
          100: "#e3e5e5",
          200: "#c3c8c8",
          300: "#9ba3a3",
          // 400 was #6f7979 originally — darkened to #657070 after
          // computing its real WCAG contrast ratio against white
          // (4.48:1, just under the 4.5:1 AA threshold for normal-size
          // text) and finding this project uses it for exactly that:
          // small (text-xs) but meaningful text like job IDs, table
          // headers, and stat labels — never purely decorative. New
          // value: 5.12:1, comfortably passing with margin.
          400: "#657070",
          500: "#525c5c",
          600: "#3d4646",
          700: "#2d3535",
          800: "#1e2424",
          900: "#131717",
          950: "#0a0d0d",
        },
      },
      fontFamily: {
        sans: ["IBM Plex Sans", "system-ui", "sans-serif"],
        mono: ["IBM Plex Mono", "ui-monospace", "monospace"],
      },
    },
  },
  plugins: [],
};
