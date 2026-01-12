interface ReviewStatusEmojiProps {
  status: 'APPROVED' | 'CHANGES_REQUESTED' | 'COMMENTED' | '' | null;
}

export function ReviewStatusEmoji({ status }: ReviewStatusEmojiProps) {
  if (!status) {
    return <span className="review-emoji" title="Pending review">📥</span>;
  }

  switch (status) {
    case 'APPROVED':
      return <span className="review-emoji" title="Approved">✅</span>;
    case 'CHANGES_REQUESTED':
      return <span className="review-emoji" title="Changes requested">🚧</span>;
    case 'COMMENTED':
      return <span className="review-emoji" title="Commented">💬</span>;
    default:
      return <span className="review-emoji" title="Pending review">📥</span>;
  }
}
