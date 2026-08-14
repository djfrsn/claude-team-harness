export function outcome(success: boolean): string {
  if (success) {
    return "success";
  }
  return "failure";
}
