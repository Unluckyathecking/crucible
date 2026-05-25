import { describe, it, expect, vi } from 'vitest';
import { SubmitButton } from './submit-button';
import * as reactDom from 'react-dom';

vi.mock('react-dom', () => ({
  useFormStatus: vi.fn()
}));

describe('SubmitButton', () => {
  it('renders default text and is not disabled when pending=false', () => {
    vi.mocked(reactDom.useFormStatus).mockReturnValue({ pending: false, data: null, method: null, action: null } as any);
    const element = SubmitButton() as any;
    expect(element.props.children.props.disabled).toBe(false);
    expect(element.props.children.props.children).toBe("Send magic link");
  });

  it('renders loading text and is disabled when pending=true', () => {
    vi.mocked(reactDom.useFormStatus).mockReturnValue({ pending: true, data: null, method: null, action: null } as any);
    const element = SubmitButton() as any;
    expect(element.props.children.props.disabled).toBe(true);
    // When pending=true, it renders a fragment with sr-only text and visible "Sending..."
    expect(element.props.children.props.children.props.children[0].props.children).toBe("Sending email,");
    expect(element.props.children.props.children.props.children[1].props.children).toBe("Sending...");
  });
});
