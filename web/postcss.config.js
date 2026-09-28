// PostCSS pipeline: Tailwind 3 generates the utilities, autoprefixer adds the
// vendor prefixes the target browsers of the seven locales still need.
export default {
  plugins: {
    tailwindcss: {},
    autoprefixer: {},
  },
};
