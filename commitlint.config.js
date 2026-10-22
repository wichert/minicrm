/**
 * Commitlint configuration: enforce Conventional Commits
 * (https://www.conventionalcommits.org/).
 *
 * Commit messages must look like:
 *   feat: add IMAP sync retry
 *   fix(workflow): preserve reopens_thread_id on merge
 *   feat!: change attachment store layout  (breaking, via "!" or footer)
 */
module.exports = {
  extends: ["@commitlint/config-conventional"],
};
