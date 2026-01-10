interface CommitShaProps {
  sha: string;
  owner: string;
  repo: string;
}

export function CommitSha({ sha, owner, repo }: CommitShaProps) {
  const shortSha = sha.substring(0, 7);
  const commitUrl = `https://github.com/${owner}/${repo}/commit/${sha}`;

  return (
    <a
      href={commitUrl}
      target="_blank"
      rel="noopener noreferrer"
      className="commit-sha"
      title={`View commit ${sha}`}
    >
      {shortSha}
    </a>
  );
}
