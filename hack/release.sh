#!/usr/bin/env bash
# Cuts a release: checks the tree, then creates and pushes an annotated
# vX.Y.Z tag on the tip of origin/main. Pushing the tag starts
# .github/workflows/release.yml, which builds, signs and publishes the
# binaries for every platform (CONTRIBUTING.md#releases).
#
#   make release TAG=v1.2.3        # or v1.2.3-rc.1 for a pre-release
#   make release TAG=v1.2.3 DRY_RUN=1
#
# The release workflow refuses a tag that is not on main; this refuses it
# earlier, before anything is pushed.
set -euo pipefail

version=${1:-}
remote=${RELEASE_REMOTE:-origin}
dry_run=${DRY_RUN:-}

die() { echo "release: $*" >&2; exit 1; }

latest=$(git tag --list 'v*' --sort=-v:refname | head -n 1)
if [ -z "$version" ]; then
	echo "usage: make release TAG=vX.Y.Z  (latest tag: ${latest:-none})" >&2
	exit 2
fi

# Semantic versioning with a leading v; an optional pre-release suffix
# (v1.2.3-rc.1) is published as a GitHub pre-release.
semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'
[[ $version =~ $semver ]] || die "$version is not vMAJOR.MINOR.PATCH[-PRERELEASE]"

[ -z "$(git status --porcelain)" ] || die "the working tree has uncommitted changes"

git fetch --quiet --tags "$remote" main
latest=$(git tag --list 'v*' --sort=-v:refname | head -n 1)
git rev-parse -q --verify "refs/tags/$version" >/dev/null && die "tag $version already exists"

commit=$(git rev-parse "$remote/main")
head=$(git rev-parse HEAD)
[ "$head" = "$commit" ] || die "HEAD ($head) is not $remote/main ($commit); check out main and pull"

# MAJOR.MINOR.PATCH must not go backwards; the same core is fine, so
# v1.2.3-rc.1 can be followed by v1.2.3-rc.2 and then v1.2.3.
core() { [[ $1 =~ $semver ]] && printf '%d %d %d' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"; }
if [ -n "$latest" ] && [[ $latest =~ $semver ]]; then
	read -r a1 a2 a3 <<<"$(core "$latest")"
	read -r b1 b2 b3 <<<"$(core "$version")"
	if ((b1 < a1 || (b1 == a1 && (b2 < a2 || (b2 == a2 && b3 < a3))))); then
		die "$version is older than the latest tag $latest"
	fi
fi

echo "release: tagging $(git log -1 --format='%h %s' "$commit") as $version"
if [ -n "$dry_run" ]; then
	echo "release: DRY_RUN set; nothing tagged or pushed"
	exit 0
fi

git tag -a "$version" -m "icaro $version" "$commit"
git push "$remote" "refs/tags/$version"

url=$(git remote get-url "$remote" | sed -E 's#^git@([^:]+):#https://\1/#; s#\.git$##')
echo "release: pushed $version; follow it at $url/actions/workflows/release.yml"
echo "release: the publishing jobs wait for approval in the 'release' environment"
